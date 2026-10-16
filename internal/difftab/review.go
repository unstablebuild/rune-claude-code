// Copyright 2026 Unstable Build, LLC.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package difftab

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/term"

	"github.com/unstablebuild/rune-claude-code/internal/ideserver"
)

// headerRows are the rows above the diff: title, keys and a rule.
const headerRows = 3

const keysHelp = " a accept   r reject   e edit, then save to accept   " +
	"q answer in the terminal"

var (
	attrTitle  = term.Attributes{Attrs: term.AttrBold}
	attrDim    = term.Attributes{Attrs: term.AttrDim}
	attrError  = term.Attributes{Fg: term.ColorRed}
	attrHunk   = term.Attributes{Fg: term.ColorTeal}
	attrDelete = term.Attributes{Fg: term.ColorRed}
	attrInsert = term.Attributes{Fg: term.ColorGreen}
)

// review is the window content showing one openDiff call. It is
// resolved once with the text blocks openDiff returns.
type review struct {
	name     string
	path     string
	title    string
	proposed string
	lines    []line
	gutter   int
	done     chan []string
	once     sync.Once
	onEdit   func(*review)

	mu      sync.Mutex
	win     browserapi.Window
	exited  bool
	editing bool
	status  string
	offset  int
	width   int
	height  int
}

var _ browserapi.Handler = (*review)(nil)

func newReview(name, path, title, old, proposed string, onEdit func(*review)) *review {
	r := &review{
		name:     name,
		path:     path,
		title:    title,
		proposed: proposed,
		lines:    diffLines(old, proposed),
		done:     make(chan []string, 1),
		onEdit:   onEdit,
	}
	maxLine := 0
	for _, l := range r.lines {
		maxLine = max(maxLine, l.old, l.new)
	}
	r.gutter = len(strconv.Itoa(maxLine))
	return r
}

// resolve answers openDiff with blocks unless it was answered already.
func (r *review) resolve(blocks ...string) {
	r.once.Do(func() { r.done <- blocks })
}

// Handle implements tui.Handler. Accepting or rejecting answers
// openDiff and closes the window; q closes it and leaves the answer to
// Claude Code's own prompt.
func (r *review) Handle(ev term.Event) (exit, handled bool) {
	if ev.Type == term.EventMouse {
		switch ev.Key {
		case term.MouseWheelUp:
			r.scroll(-3)
			return false, true
		case term.MouseWheelDown:
			r.scroll(3)
			return false, true
		}
		return false, false
	}
	if ev.Type != term.EventKey || ev.Mod&^term.ModShift != 0 {
		return false, false
	}
	page := max(r.bodyHeight()-1, 1)
	switch {
	case ev.Ch == 'a' || ev.Ch == 'y':
		r.resolve(ideserver.DiffTabClosed)
		return r.exit()
	case ev.Ch == 'r' || ev.Ch == 'n':
		r.resolve(ideserver.DiffRejected)
		return r.exit()
	case ev.Ch == 'q' || ev.Key == term.KeyEsc:
		return r.exit()
	case ev.Ch == 'e':
		r.mu.Lock()
		start := !r.editing && r.win != nil
		if start {
			r.editing = true
		}
		r.mu.Unlock()
		if start {
			// The edit buffer is opened through the host, which is
			// waiting for this call to return.
			go r.onEdit(r)
		}
	case ev.Ch == 'j' || ev.Key == term.KeyArrowDown:
		r.scroll(1)
	case ev.Ch == 'k' || ev.Key == term.KeyArrowUp:
		r.scroll(-1)
	case ev.Ch == ' ' || ev.Key == term.KeySpace || ev.Key == term.KeyPgdn:
		r.scroll(page)
	case ev.Ch == 'b' || ev.Key == term.KeyPgup:
		r.scroll(-page)
	case ev.Ch == 'g' || ev.Key == term.KeyHome:
		r.scroll(-len(r.lines))
	case ev.Ch == 'G' || ev.Key == term.KeyEnd:
		r.scroll(len(r.lines))
	default:
		return false, false
	}
	return false, true
}

func (r *review) exit() (bool, bool) {
	r.mu.Lock()
	r.exited = true
	r.mu.Unlock()
	return true, true
}

// editFailed reports err and lets the user try again.
func (r *review) editFailed(err error) {
	r.mu.Lock()
	r.editing = false
	r.status = " Cannot edit: " + err.Error()
	r.mu.Unlock()
}

func (r *review) bodyHeight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.height - headerRows
}

func (r *review) scroll(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	maxOffset := max(len(r.lines)-(r.height-headerRows), 0)
	r.offset = min(max(r.offset+n, 0), maxOffset)
}

// Resize implements tui.Component.
func (r *review) Resize(width, height int) {
	r.mu.Lock()
	r.width, r.height = width, height
	r.mu.Unlock()
	r.scroll(0)
}

// Draw implements tui.Component.
func (r *review) Draw(w term.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	y := 0
	put := func(text string, attr term.Attributes) {
		if y < r.height {
			drawLine(w, y, r.width, text, attr)
		}
		y++
	}
	put(" Claude Code wants to edit "+r.title, attrTitle)
	if r.status != "" {
		put(r.status, attrError)
	} else {
		put(keysHelp, attrDim)
	}
	put(strings.Repeat("─", r.width), attrDim)
	if len(r.lines) == 0 {
		put(" No changes.", attrDim)
		return
	}
	for _, l := range r.lines[r.offset:] {
		if y >= r.height {
			break
		}
		put(r.format(l))
	}
}

func (r *review) format(l line) (string, term.Attributes) {
	num := func(n int) string {
		if n == 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	gutter := fmt.Sprintf(" %*s %*s ", r.gutter, num(l.old), r.gutter, num(l.new))
	switch l.kind {
	case lineHunk:
		return " " + l.text, attrHunk
	case lineDelete:
		return gutter + "- " + l.text, attrDelete
	case lineInsert:
		return gutter + "+ " + l.text, attrInsert
	default:
		return gutter + "  " + l.text, term.Attributes{}
	}
}

// drawLine writes text on row y, one cell per rune, expanding tabs and
// clipping at width.
func drawLine(w term.Writer, y, width int, text string, attr term.Attributes) {
	x := 0
	set := func(ch rune) {
		if x < width {
			w.SetCell(term.Coordinates{X: x, Y: y}, term.NewCell(ch, 1, attr))
		}
		x++
	}
	for _, ch := range text {
		if ch == '\t' {
			for range 4 - x%4 {
				set(' ')
			}
			continue
		}
		set(ch)
	}
}

// Cursor implements tui.Handler; the review has no cursor.
func (r *review) Cursor() (term.Coordinates, term.CursorStyle, bool) {
	return term.Coordinates{}, 0, false
}

// Selection implements tui.Handler.
func (r *review) Selection() (string, bool) { return "", false }

// Close implements browserapi.Handler. The host calls it when the
// review leaves its window; that alone does not answer openDiff.
func (r *review) Close() error { return nil }

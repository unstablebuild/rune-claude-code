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
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/term"

	"github.com/unstablebuild/rune-claude-code/internal/ideserver"
)

const (
	// footerRows are the rows below the diff: a rule and the buttons.
	footerRows = 2
	// signWidth is the cells of the gutter's "+" or "-" and the space
	// after it; the line numbers follow, as in Rune's editor.
	signWidth = 2
	// editColumns is the fewest columns of text a review asks its
	// window for, which Edit opens the proposal in.
	editColumns = 90
	// editChrome is the cells Rune's editor takes besides its line
	// numbers and text: the icon and fold bars, and the scroll bar.
	editChrome = 2 + 2 + 1 + 1
	// minHeight is the fewest rows a review asks its window for.
	minHeight = 12
	// jumpMargin is the lines ] and [ leave above the change they jump to.
	jumpMargin = 3
	// titlePad is the cells of the window bar around the title.
	titlePad = 6
	// panStep is the cells h and l scroll sideways.
	panStep = 4
	// buttonPad is the space on each side of a button's label.
	buttonPad = "  "

	hint = "q answers in the terminal"
)

type action uint8

const (
	actionAccept action = iota
	actionReject
	actionEdit
)

// buttons are the bottom bar's, left to right. The first letter of
// each label is its key.
var buttons = []struct {
	label  string
	action action
}{
	{"Accept", actionAccept},
	{"Reject", actionReject},
	{"Edit", actionEdit},
}

// review is the window content showing one openDiff call. It is
// resolved once with the text blocks openDiff returns.
type review struct {
	name     string
	path     string
	title    string
	old      string
	proposed string
	theme    Theme
	// all is every line of the diff.
	all []line
	// numWidth is the cells of the gutter's line numbers, textWidth of
	// the widest line, footerWidth of the bottom bar, and editGutter of
	// the gutter and scroll bar of the editor Edit opens.
	numWidth, textWidth, footerWidth, editGutter int
	inserted, deleted                            int
	// firstChange is the 0-based line of the proposal the first change
	// is on, where Edit puts the cursor.
	firstChange int
	done        chan []string
	once        sync.Once
	onEdit      func(*review)

	mu      sync.Mutex
	win     browserapi.Window
	exited  bool
	editing bool
	status  string
	// lines are all as shown, with the unchanged lines that do not fit
	// in the window folded; laidOut is the rows they were folded for.
	lines   []line
	laidOut int
	// changes are the indexes in lines of the first row of each change.
	changes []int
	// remark marks the changes of the proposal being edited again.
	remark *time.Timer
	// offset is the first line shown and left the first cell of text.
	offset, left  int
	width, height int
	oldHL, newHL  highlights
	// selected is the highlighted button, which Enter presses; pressed
	// is the one the mouse went down on, -1 for none.
	selected, pressed int
	mouseDown         bool
}

var _ browserapi.Floating = (*review)(nil)

func newReview(name, path, title, old, proposed string, theme Theme, onEdit func(*review)) *review {
	r := &review{
		name:     name,
		path:     path,
		title:    title,
		old:      old,
		proposed: proposed,
		theme:    theme,
		all:      diffAll(old, proposed),
		done:     make(chan []string, 1),
		onEdit:   onEdit,
		pressed:  -1,
	}
	maxLine := 0
	for _, l := range r.all {
		switch l.kind {
		case lineInsert:
			r.inserted++
		case lineDelete:
			r.deleted++
		}
		maxLine = max(maxLine, l.new)
		r.textWidth = max(r.textWidth, r.cells(l.text))
	}
	digits := len(strconv.Itoa(maxLine))
	r.numWidth = digits + 1
	r.editGutter = digits + 1 + editChrome
	r.footerWidth = 1 + len(r.stats()) + 3 + len(hint) + 1 + buttonsWidth()
	r.firstChange = firstChange(r.all)
	r.lines = fold(r.all, minContext)
	r.changes = changesOf(r.lines)
	r.laidOut = -1
	return r
}

// firstChange is the 0-based line of the proposal its first change is
// on: the first line inserted, or the one after the first deleted.
func firstChange(all []line) int {
	changed, last := false, 0
	for _, l := range all {
		changed = changed || l.kind != lineEqual
		if l.new > 0 {
			if changed {
				return l.new - 1
			}
			last = l.new - 1
		}
	}
	return last
}

// changesOf returns the indexes in lines of the first row of each
// change.
func changesOf(lines []line) []int {
	var changes []int
	for i, l := range lines {
		if isChange(l) && (i == 0 || !isChange(lines[i-1])) {
			changes = append(changes, i)
		}
	}
	return changes
}

func isChange(l line) bool { return l.kind == lineInsert || l.kind == lineDelete }

// stats counts the changed lines for the bottom bar.
func (r *review) stats() string { return fmt.Sprintf("+%d -%d", r.inserted, r.deleted) }

func foldText(l line) string {
	if l.folded == 1 {
		return "⋯ 1 unchanged line"
	}
	return fmt.Sprintf("⋯ %d unchanged lines", l.folded)
}

// cells is the width of text in the editor, where a tab takes
// TabSpaces cells.
func (r *review) cells(text string) int {
	n := 0
	for _, ch := range text {
		if ch == '\t' {
			n += r.theme.TabSpaces
		} else {
			n++
		}
	}
	return n
}

// resolve answers openDiff with blocks unless it was answered already.
func (r *review) resolve(blocks ...string) {
	r.once.Do(func() { r.done <- blocks })
}

func (r *review) setHighlights(old, proposed highlights) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.oldHL, r.newHL = old, proposed
}

// Dimensions implements component.Floating. A review asks for room
// for every line of the diff, unfolded, and for at least editColumns of
// text in the editor Edit opens in the same window; Rune shrinks the
// window to fit its own, and Resize folds what does not fit.
func (r *review) Dimensions() (width, height int) {
	text := max(r.textWidth, editColumns)
	width = max(signWidth+r.numWidth+text+1, r.editGutter+text,
		r.footerWidth, utf8.RuneCountInString(r.title)+titlePad)
	return width, max(len(r.all)+footerRows, minHeight)
}

// Handle implements tui.Handler. Accepting or rejecting answers
// openDiff and closes the window; q closes it and leaves the answer to
// Claude Code's own prompt.
func (r *review) Handle(ev term.Event) (exit, handled bool) {
	if ev.Type == term.EventMouse {
		return r.mouse(ev)
	}
	if ev.Type != term.EventKey {
		return false, false
	}
	// As in Rune's prompts, Ctrl-h and Ctrl-l move the selection, and
	// Emacs' Ctrl-b and Ctrl-f too; at either end the key is Rune's.
	if ev.Mod == term.ModCtrl {
		switch ev.Ch {
		case 'h', 'b':
			return false, r.selectBy(-1)
		case 'l', 'f':
			return false, r.selectBy(1)
		}
	}
	if ev.Mod&^term.ModShift != 0 {
		return false, false
	}
	page := max(r.bodyRows()-1, 1)
	switch {
	case ev.Ch == 'a' || ev.Ch == 'y':
		return r.act(actionAccept)
	case ev.Ch == 'r' || ev.Ch == 'n':
		return r.act(actionReject)
	case ev.Ch == 'e':
		return r.act(actionEdit)
	case ev.Ch == 'q' || ev.Key == term.KeyEsc:
		return r.exit()
	case ev.Key == term.KeyEnter:
		r.mu.Lock()
		selected := r.selected
		r.mu.Unlock()
		return r.act(buttons[selected].action)
	case ev.Key == term.KeyArrowLeft:
		return false, r.selectBy(-1)
	case ev.Key == term.KeyArrowRight:
		return false, r.selectBy(1)
	case ev.Ch == 'j' || ev.Key == term.KeyArrowDown:
		r.scroll(1)
	case ev.Ch == 'k' || ev.Key == term.KeyArrowUp:
		r.scroll(-1)
	case ev.Ch == 'l':
		r.pan(panStep)
	case ev.Ch == 'h':
		r.pan(-panStep)
	case ev.Ch == ']':
		r.jump(1)
	case ev.Ch == '[':
		r.jump(-1)
	case ev.Ch == ' ' || ev.Key == term.KeySpace || ev.Key == term.KeyPgdn:
		r.scroll(page)
	case ev.Ch == 'b' || ev.Key == term.KeyPgup:
		r.scroll(-page)
	case ev.Ch == 'g' || ev.Key == term.KeyHome:
		r.scroll(-len(r.all))
	case ev.Ch == 'G' || ev.Key == term.KeyEnd:
		r.scroll(len(r.all))
	default:
		return false, false
	}
	return false, true
}

// selectBy moves the selection n buttons right, or left, and reports
// whether there was a button there.
func (r *review) selectBy(n int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := r.selected + n
	if next < 0 || next >= len(buttons) {
		return false
	}
	r.selected = next
	return true
}

// mouse scrolls with the wheel, selects the button under the pointer,
// and presses it when the mouse goes down and up on it.
func (r *review) mouse(ev term.Event) (exit, handled bool) {
	switch ev.Key {
	case term.MouseWheelUp:
		r.scroll(-3)
		return false, true
	case term.MouseWheelDown:
		r.scroll(3)
		return false, true
	case term.MouseLeft:
		r.mu.Lock()
		defer r.mu.Unlock()
		at := r.pointAt(ev.MouseX, ev.MouseY)
		// Rune repeats MouseLeft while the mouse drags.
		if !r.mouseDown {
			r.mouseDown, r.pressed = true, at
		}
		return false, true
	case term.MouseRelease:
		r.mu.Lock()
		at := r.pointAt(ev.MouseX, ev.MouseY)
		pressed := r.pressed
		r.pressed, r.mouseDown = -1, false
		r.mu.Unlock()
		if at >= 0 && at == pressed {
			return r.act(buttons[at].action)
		}
		return false, true
	case 0:
		// The pointer moved, or left the window.
		r.mu.Lock()
		defer r.mu.Unlock()
		r.pointAt(ev.MouseX, ev.MouseY)
		return false, true
	}
	return false, false
}

// pointAt selects the button at x, y, if there is one, and returns its
// index or -1. The selection stays when the pointer leaves the buttons.
func (r *review) pointAt(x, y int) int {
	at := r.buttonAt(x, y)
	if at >= 0 {
		r.selected = at
	}
	return at
}

func (r *review) act(a action) (exit, handled bool) {
	switch a {
	case actionAccept:
		r.resolve(ideserver.DiffTabClosed)
		return r.exit()
	case actionReject:
		r.resolve(ideserver.DiffRejected)
		return r.exit()
	}
	r.mu.Lock()
	start := !r.editing && r.win != nil
	if start {
		r.editing = true
	}
	r.mu.Unlock()
	if start {
		// The edit buffer is opened through the host, which is waiting
		// for this call to return.
		go r.onEdit(r)
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
	r.status = "Cannot edit: " + err.Error()
	r.mu.Unlock()
}

// bodyRows is the rows the diff takes.
func (r *review) bodyRows() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bodyRowsLocked()
}

func (r *review) bodyRowsLocked() int { return max(r.height-footerRows, 0) }

func (r *review) scroll(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setOffsetLocked(r.offset + n)
}

func (r *review) setOffsetLocked(n int) {
	r.offset = min(max(n, 0), max(len(r.lines)-r.bodyRowsLocked(), 0))
}

func (r *review) pan(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setLeftLocked(r.left + n)
}

func (r *review) setLeftLocked(n int) {
	visible := r.width - signWidth - r.numWidth - 1
	r.left = min(max(n, 0), max(r.textWidth-visible, 0))
}

// jump scrolls to the next change below the one in view, or to the
// previous one above it, leaving jumpMargin lines above the change.
func (r *review) jump(dir int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at := r.offset + jumpMargin
	target := -1
	if dir > 0 {
		for _, c := range r.changes {
			if c > at {
				target = c
				break
			}
		}
	} else {
		for _, c := range slices.Backward(r.changes) {
			if c < at {
				target = c
				break
			}
		}
	}
	if target >= 0 {
		r.setOffsetLocked(target - jumpMargin)
	}
}

// Resize implements tui.Component.
func (r *review) Resize(width, height int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.width, r.height = width, height
	r.layoutLocked(r.bodyRowsLocked())
	r.setOffsetLocked(r.offset)
	r.setLeftLocked(r.left)
}

// layoutLocked folds the unchanged lines that do not fit in body rows,
// showing as many around each change as fit, and at least minContext.
func (r *review) layoutLocked(body int) {
	if body == r.laidOut {
		return
	}
	r.laidOut = body
	// More context never takes fewer rows.
	best := minContext
	for lo, hi := minContext+1, len(r.all); lo <= hi; {
		mid := (lo + hi) / 2
		if len(fold(r.all, mid)) <= body {
			best, lo = mid, mid+1
		} else {
			hi = mid - 1
		}
	}
	r.lines = fold(r.all, best)
	r.changes = changesOf(r.lines)
}

// Draw implements tui.Component.
func (r *review) Draw(w term.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	body := r.bodyRowsLocked()
	for y := range body {
		if i := r.offset + y; i < len(r.lines) {
			r.drawLine(w, y, r.lines[i])
		} else {
			fillRow(w, y, r.width, r.theme.Text)
		}
	}
	if len(r.lines) == 0 && body > 0 {
		putText(w, signWidth, 0, r.width, "No changes.", r.dim())
	}
	r.drawFooter(w)
}

// dim is the attribute of the gutter's line numbers on plain text.
func (r *review) dim() term.Attributes { return over(r.theme.Text, fgOf(r.theme.LineNumber)) }

func (r *review) drawLine(w term.Writer, y int, l line) {
	base := r.theme.Text
	var word term.Attributes
	var hl []styled
	switch l.kind {
	case lineInsert:
		base, word, hl = over(base, r.theme.Insert), r.theme.InsertWord, r.newHL[l.new-1]
	case lineDelete:
		base, word, hl = over(base, r.theme.Delete), r.theme.DeleteWord, r.oldHL[l.old-1]
	case lineEqual:
		hl = r.newHL[l.new-1]
	}
	fillRow(w, y, r.width, base)
	text := signWidth + r.numWidth
	if l.kind == lineFold {
		putText(w, text, y, r.width, foldText(l), r.dim())
		return
	}
	sign := over(base, term.Attributes{Attrs: term.AttrBold})
	switch l.kind {
	case lineInsert:
		putText(w, 0, y, r.width, "+", sign)
	case lineDelete:
		putText(w, 0, y, r.width, "-", sign)
	}
	if l.kind != lineDelete {
		putText(w, signWidth, y, r.width, strconv.Itoa(l.new), over(base, fgOf(r.theme.LineNumber)))
	}

	runes := []rune(l.text)
	attrs := make([]term.Attributes, len(runes))
	for i := range attrs {
		attrs[i] = base
	}
	// Syntax colors the text and keeps the line's tint, as Rune's git
	// diff does.
	for _, s := range hl {
		for i := s.from; i < min(s.to, len(runes)); i++ {
			attrs[i] = over(attrs[i], fgOf(s.attr))
		}
	}
	for _, s := range l.changed {
		for i := s.from; i < min(s.to, len(runes)); i++ {
			attrs[i] = over(attrs[i], term.Attributes{Bg: word.Bg})
		}
	}
	col := -r.left
	for i, ch := range runes {
		n := 1
		if ch == '\t' {
			ch, n = ' ', r.theme.TabSpaces
		}
		for range n {
			if x := text + col; x >= text && x < r.width {
				w.SetCell(term.Coordinates{X: x, Y: y}, term.NewCell(ch, 1, attrs[i]))
			}
			col++
		}
		if text+col >= r.width {
			break
		}
	}
}

// drawFooter draws the rule and the bottom bar: what changed, or why
// editing failed, and the buttons.
func (r *review) drawFooter(w term.Writer) {
	if y := r.height - 2; y >= 0 {
		for x := range r.width {
			w.SetCell(term.Coordinates{X: x, Y: y}, term.NewCell('─', 1, r.dim()))
		}
	}
	y := r.height - 1
	if y < 0 {
		return
	}
	fillRow(w, y, r.width, r.theme.Text)
	rects := buttonsAt(r.width)
	limit := rects[0].x - 1
	if r.status != "" {
		putText(w, 1, y, limit, r.status, over(r.theme.Text, term.Attributes{Fg: term.ColorRed}))
	} else {
		x := putText(w, 1, y, limit, fmt.Sprintf("+%d", r.inserted), over(r.theme.Text, r.theme.Inserted))
		x = putText(w, x+1, y, limit, fmt.Sprintf("-%d", r.deleted), over(r.theme.Text, r.theme.Deleted))
		if x+3+len(hint) <= limit {
			putText(w, x+3, y, limit, hint, r.dim())
		}
	}
	for i, b := range buttons {
		attr := r.theme.Button
		if i == r.selected {
			attr = r.theme.Selected
		}
		attr = over(r.theme.Text, attr)
		key := attr
		key.Attrs |= term.AttrUnderline
		x := putText(w, rects[i].x, y, r.width, buttonPad, attr)
		x = putText(w, x, y, r.width, b.label[:1], key)
		x = putText(w, x, y, r.width, b.label[1:], attr)
		putText(w, x, y, r.width, buttonPad, attr)
	}
}

// rect is the cells [x, x+w) of the bottom bar a button takes.
type rect struct{ x, w int }

// buttonsAt lays the buttons out at the right of a bottom bar width
// wide, a cell apart and a cell from its edge.
func buttonsAt(width int) []rect {
	rects := make([]rect, len(buttons))
	x := width - 1
	for i := len(buttons) - 1; i >= 0; i-- {
		w := 2*len(buttonPad) + len(buttons[i].label)
		x -= w
		rects[i] = rect{x: x, w: w}
		x--
	}
	return rects
}

func buttonsWidth() int {
	rects := buttonsAt(0)
	return -rects[0].x
}

// buttonAt is the index of the button at x, y, or -1.
func (r *review) buttonAt(x, y int) int {
	if y != r.height-1 {
		return -1
	}
	for i, b := range buttonsAt(r.width) {
		if x >= b.x && x < b.x+b.w {
			return i
		}
	}
	return -1
}

// Cursor implements tui.Handler; the review has no cursor.
func (r *review) Cursor() (term.Coordinates, term.CursorStyle, bool) {
	return term.Coordinates{}, 0, false
}

// Selection implements tui.Handler.
func (r *review) Selection() (string, bool) { return "", false }

// Close implements browserapi.Handler. The host calls it when the
// review leaves its window: when the edit buffer replaces it, or when
// the window is closed from its bar, which leaves the answer to Claude
// Code's own prompt as q does. Neither answers openDiff.
func (r *review) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.editing {
		r.exited = true
	}
	return nil
}

// over returns base with the colors top sets and the flags of both.
func over(base, top term.Attributes) term.Attributes {
	if top.Fg.Valid() {
		base.Fg = top.Fg
	}
	if top.Bg.Valid() {
		base.Bg = top.Bg
	}
	if top.Underline.Valid() {
		base.Underline = top.Underline
	}
	base.Attrs |= top.Attrs
	return base
}

// fgOf is a without its background, to draw over a line's tint.
func fgOf(a term.Attributes) term.Attributes {
	a.Bg = term.ColorDefault
	return a
}

func fillRow(w term.Writer, y, width int, attr term.Attributes) {
	for x := range width {
		w.SetCell(term.Coordinates{X: x, Y: y}, term.NewCell(' ', 1, attr))
	}
}

// putText writes text on row y from x, one cell per rune, clipping
// before limit, and returns the cell after it.
func putText(w term.Writer, x, y, limit int, text string, attr term.Attributes) int {
	for _, ch := range text {
		if x >= 0 && x < limit {
			w.SetCell(term.Coordinates{X: x, Y: y}, term.NewCell(ch, 1, attr))
		}
		x++
	}
	return x
}

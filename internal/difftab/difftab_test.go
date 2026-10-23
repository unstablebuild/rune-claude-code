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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/syntaxapi"
	"github.com/unstablebuild/rune-go-sdk/api/textapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/component"
	"github.com/unstablebuild/rune-go-sdk/iterator"
	"github.com/unstablebuild/rune-go-sdk/term"

	"github.com/unstablebuild/rune-claude-code/internal/ideserver"
)

const target = "/ws/pkg/main.go"

func TestDiffLines(t *testing.T) {
	diffLines := func(a, b string) []line { return fold(diffAll(a, b), 3) }
	old := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n"
	proposed := "1\nTWO\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n"
	assert.Equal(t, []string{
		"  1 1 1",
		"- 2 . 2",
		"+ . 2 TWO",
		"  3 3 3",
		"  4 4 4",
		"  5 5 5",
		"⋯ 6 6 4",
		"  10 10 10",
		"  11 11 11",
		"  12 12 12",
		"+ . 13 13",
	}, describe(diffLines(old, proposed)))

	assert.Empty(t, diffLines("same\n", "same\n"))

	assert.Equal(t, []string{"+ . 1 a", "+ . 2 ", "+ . 3 b"}, describe(diffLines("", "a\n\nb")))

	// Unchanged lines before the first change fold too.
	assert.Equal(t, []string{
		"⋯ 1 1 6", "  7 7 g", "  8 8 h", "  9 9 i", "- 10 . j", "+ . 10 J",
	}, describe(diffLines("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n", "a\nb\nc\nd\ne\nf\ng\nh\ni\nJ\n")))

	// One unchanged line between two changes is shown rather than folded.
	got := diffLines("1\n2\n3\n4\n5\n6\n7\n8\n9\n", "ONE\n2\n3\n4\n5\n6\n7\n8\nNINE\n")
	require.Len(t, got, 11)
	assert.Equal(t, "  5 5 5", describe(got)[5])
}

func TestChangedSpans(t *testing.T) {
	lines := diffAll("x := 1\ny\n", "x := 2\ny\n")
	require.Len(t, lines, 3)
	assert.Equal(t, []span{{5, 6}}, lines[0].changed)
	assert.Equal(t, []span{{5, 6}}, lines[1].changed)

	o, n := changedSpans("return a + b", "return a - b")
	assert.Equal(t, []span{{9, 10}}, o)
	assert.Equal(t, []span{{9, 10}}, n)

	o, n = changedSpans("foo", "bar")
	assert.Nil(t, o, "lines with too little in common are not marked")
	assert.Nil(t, n)
}

func TestChangeLocations(t *testing.T) {
	added, deleted := changeLocations(diffAll("a\nb\nc\nd\ne\n", "a\nB\nB2\nc\nd\n"), DefaultTheme)
	assert.Equal(t, []textapi.Location{{
		From: term.Coordinates{Y: 1}, To: term.Coordinates{Y: 3}, Attr: DefaultTheme.InsertMark, Icon: "+",
	}}, added)
	assert.Equal(t, []textapi.Location{{
		From: term.Coordinates{Y: 1}, To: term.Coordinates{Y: 1, X: 1}, Attr: DefaultTheme.DeleteMark, Icon: "-",
		Message: "Deleted: b",
	}, {
		From: term.Coordinates{Y: 4}, To: term.Coordinates{Y: 4, X: 1}, Attr: DefaultTheme.DeleteMark, Icon: "-",
		Message: "Deleted: e",
	}}, deleted, "a deletion at the end is marked on the last line")

	_, deleted = changeLocations(diffAll("a\n  b\nc\nd\n", "a\nd\n"), DefaultTheme)
	assert.Equal(t, []textapi.Location{{
		From: term.Coordinates{Y: 1}, To: term.Coordinates{Y: 1, X: 1}, Attr: DefaultTheme.DeleteMark, Icon: "-",
		Message: "Deleted 2 lines: b …",
	}}, deleted, "on the line after it")
}

func TestCellsText(t *testing.T) {
	assert.Equal(t, "a\tb\n\nc\n", cellsText(cells("a\tb\n\nc"), true))
	assert.Equal(t, "a\nb", cellsText(cells("a\nb"), false))
	assert.Equal(t, "a\nb\n", cellsText(cells("a\nb\n"), true), "a last empty row is the line break")
}

// cells lays text out as an editor's cells, one row per line.
func cells(text string) [][]term.Cell {
	var rows [][]term.Cell
	for _, l := range strings.Split(text, "\n") {
		row := []term.Cell{}
		for _, ch := range l {
			row = append(row, term.NewCell(ch, 1, term.Attributes{}))
		}
		rows = append(rows, row)
	}
	return rows
}

func describe(lines []line) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		switch l.kind {
		case lineEqual:
			out = append(out, fmt.Sprintf("  %d %d %s", l.old, l.new, l.text))
		case lineDelete:
			out = append(out, fmt.Sprintf("- %d . %s", l.old, l.text))
		case lineInsert:
			out = append(out, fmt.Sprintf("+ . %d %s", l.new, l.text))
		case lineFold:
			out = append(out, fmt.Sprintf("⋯ %d %d %d", l.old, l.new, l.folded))
		}
	}
	return out
}

func TestOpenDiffAnswers(t *testing.T) {
	tests := []struct {
		name       string
		answer     func(*testing.T, *fixture, *review)
		want       []string
		wantClosed bool
	}{{
		name:   "accept",
		answer: func(t *testing.T, _ *fixture, r *review) { press(t, r, 'a', true) },
		want:   []string{ideserver.DiffTabClosed},
	}, {
		name:   "reject",
		answer: func(t *testing.T, _ *fixture, r *review) { press(t, r, 'r', true) },
		want:   []string{ideserver.DiffRejected},
	}, {
		name: "close_tab",
		answer: func(t *testing.T, f *fixture, r *review) {
			callTool(t, f.srv, "close_tab", map[string]any{"tab_name": r.name})
		},
		want:       []string{ideserver.DiffTabClosed},
		wantClosed: true,
	}, {
		name: "closeAllDiffTabs",
		answer: func(t *testing.T, f *fixture, _ *review) {
			callTool(t, f.srv, "closeAllDiffTabs", nil)
		},
		want:       []string{ideserver.DiffTabClosed},
		wantClosed: true,
	}, {
		name: "q leaves the answer to the terminal",
		answer: func(t *testing.T, f *fixture, r *review) {
			press(t, r, 'q', true)
			callTool(t, f.srv, "close_tab", map[string]any{"tab_name": r.name})
		},
		want: []string{ideserver.DiffTabClosed},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			res, r := f.openDiff(t, context.Background())
			tt.answer(t, f, r)
			assert.Equal(t, tt.want, texts(t, <-res))
			if tt.wantClosed {
				assert.Equal(t, []uint64{2}, f.wm.closedIDs())
			} else {
				assert.Empty(t, f.wm.closedIDs())
			}
			f.fs.assertUntouched(t)
		})
	}
}

func TestOpenDiffEdit(t *testing.T) {
	f := newFixture(t)
	res, r := f.openDiff(t, context.Background())
	press(t, r, 'e', false)

	var shown browserapi.Handler
	select {
	case shown = <-f.wm.content:
	case <-time.After(5 * time.Second):
		t.Fatal("edit buffer not shown")
	}
	opened := f.opener.last()
	assert.Equal(t, browserapi.Handler(fakeHandler{uri: opened}), shown)
	path, err := workspaceapi.ParseURI(opened)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(path.Path(), "/tmp/claude-review-"), path.Path())
	assert.True(t, strings.HasSuffix(path.Path(), "/main.go"), path.Path())
	assert.Equal(t, "package main\n// proposed\n", f.fs.get(path.Path()))
	require.Eventually(t, func() bool { return len(f.editor.cursors(opened)) > 0 }, 5*time.Second, time.Millisecond)
	assert.Equal(t, []term.Coordinates{{Y: 1}}, f.editor.cursors(opened), "the cursor is on the change")
	assert.Equal(t, []textapi.Location{{
		From: term.Coordinates{Y: 1}, To: term.Coordinates{Y: 2}, Attr: DefaultTheme.InsertMark, Icon: "+",
	}}, f.editor.locations(opened, addedID), "the inserted line is marked")
	assert.Empty(t, f.editor.locations(opened, deletedID))

	// Rune drops the marks on every edit; they are set again, for what
	// the proposal is now.
	f.fs.put(path.Path(), "// edited\n")
	f.mgr.Handle(context.Background(), textapi.Event{Type: textapi.EventTypeEdit, URI: path})
	require.Eventually(t, func() bool { return len(f.editor.locations(opened, deletedID)) > 0 }, 5*time.Second, time.Millisecond)
	assert.Equal(t, []textapi.Location{{
		From: term.Coordinates{}, To: term.Coordinates{X: 1}, Attr: DefaultTheme.DeleteMark, Icon: "-",
		Message: "Deleted: package main",
	}}, f.editor.locations(opened, deletedID))
	assert.Equal(t, []textapi.Location{{
		From: term.Coordinates{}, To: term.Coordinates{Y: 1}, Attr: DefaultTheme.InsertMark, Icon: "+",
	}}, f.editor.locations(opened, addedID))

	// Other buffers are ignored.
	f.mgr.Handle(context.Background(), textapi.Event{
		Type: textapi.EventTypeFlush, URI: mustURI(t, "file://"+target), Content: "x",
	})
	f.mgr.Handle(context.Background(), textapi.Event{
		Type: textapi.EventTypeFlush, URI: path, Content: "package main\n// edited\n",
	})
	assert.Equal(t, []string{ideserver.DiffFileSaved, "package main\n// edited\n"}, texts(t, <-res))
	assert.Equal(t, []uint64{2}, f.wm.closedIDs())

	f.mgr.Handle(context.Background(), textapi.Event{Type: textapi.EventTypeClose, URI: path})
	assert.Empty(t, f.fs.paths("/tmp/"), "the proposal is removed")
	f.fs.assertUntouched(t)
}

func TestOpenDiffEditFails(t *testing.T) {
	f := newFixture(t)
	f.opener.err = os.ErrPermission
	res, r := f.openDiff(t, context.Background())
	r.Resize(80, 10)
	press(t, r, 'e', false)
	require.Eventually(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.status != "" && !r.editing
	}, 5*time.Second, time.Millisecond)
	assert.Contains(t, f.screen(r), "Cannot edit")
	assert.Positive(t, f.interrupts.Load())
	assert.Empty(t, f.fs.paths("/tmp/"), "the proposal is removed")

	press(t, r, 'a', true)
	assert.Equal(t, []string{ideserver.DiffTabClosed}, texts(t, <-res))
}

func TestOpenDiffDisconnect(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	res, _ := f.openDiff(t, ctx)
	cancel()
	got := <-res
	assert.Nil(t, got.Content)
	assert.Equal(t, []uint64{2}, f.wm.closedIDs())
}

func TestOpenDiffSameTabName(t *testing.T) {
	f := newFixture(t)
	first, _ := f.openDiff(t, context.Background())
	second, r := f.openDiff(t, context.Background())
	assert.Equal(t, []string{ideserver.DiffTabClosed}, texts(t, <-first))
	press(t, r, 'r', true)
	assert.Equal(t, []string{ideserver.DiffRejected}, texts(t, <-second))
}

// footer is the fixture's bottom bar at width 66, which its window asks for.
const footer = " +1 -0   q answers in the terminal   Accept     Reject     Edit"

func TestReviewDraw(t *testing.T) {
	f := newFixture(t)
	_, r := f.openDiff(t, context.Background())
	w, h := r.Dimensions()
	assert.Equal(t, []int{editColumns + 1 + 1 + editChrome, minHeight}, []int{w, h},
		"room for 90 columns of text in the editor, beside its gutter")
	assert.Equal(t, "Claude Code wants to edit pkg/main.go", f.wm.lastConfig().Title)
	assert.Equal(t, component.AlignmentCentered, f.wm.lastConfig().Alignment)

	r.Resize(66, 4)
	s := f.draw(r)
	assert.Equal(t, strings.Join([]string{
		"  1 package main",
		"+ 2 // proposed",
		strings.Repeat("─", 66),
		footer,
	}, "\n"), s.String(66, 4))
	assert.Equal(t, DefaultTheme.Insert.Bg, s.at(4, 1).Bg, "inserted lines are tinted")
	assert.Equal(t, DefaultTheme.Insert.Bg, s.at(60, 1).Bg, "to the window's edge")
	assert.False(t, s.at(4, 0).Bg.Valid(), "unchanged lines are not")
	assert.Equal(t, DefaultTheme.LineNumber.Fg, s.at(2, 0).Fg)
	assert.Equal(t, DefaultTheme.Selected.Bg, s.at(37, 3).Bg, "accept is selected first")
	assert.Equal(t, DefaultTheme.Button.Bg, s.at(48, 3).Bg)
	assert.NotZero(t, s.at(37, 3).Attrs&term.AttrUnderline, "a button's key is underlined")
	assert.Zero(t, s.at(38, 3).Attrs&term.AttrUnderline)

	// One row for two lines of diff.
	r.Resize(66, 3)
	press(t, r, 'j', false)
	assert.True(t, strings.HasPrefix(f.screen(r), "+ 2 // proposed\n"))
	press(t, r, 'j', false)
	assert.True(t, strings.HasPrefix(f.screen(r), "+ 2 // proposed\n"))
	press(t, r, 'g', false)
	assert.True(t, strings.HasPrefix(f.screen(r), "  1 package main\n"))
	press(t, r, 'G', false)
	assert.True(t, strings.HasPrefix(f.screen(r), "+ 2 // proposed\n"))

	// Long lines scroll sideways, the gutter stays.
	r.Resize(10, 4)
	press(t, r, 'l', false)
	assert.True(t, strings.HasPrefix(f.screen(r), "  1 age ma\n+ 2 ropose\n"), f.screen(r))
	press(t, r, 'h', false)
	assert.True(t, strings.HasPrefix(f.screen(r), "  1 packag\n"))

	exit, handled := r.Handle(term.Event{Type: term.EventKey, Ch: 'a', Mod: term.ModCtrl})
	assert.False(t, exit || handled, "modified keys are left to Rune")
}

func TestReviewDimensions(t *testing.T) {
	long := strings.Repeat("x", 200)
	r := newReview("t", target, "title", "a\n", long+"\n\tb\n", DefaultTheme, nil)
	w, h := r.Dimensions()
	assert.Equal(t, 2+editChrome+200, w, "long lines widen the window")
	assert.Equal(t, minHeight, h)

	old := strings.Repeat("line\n", 100)
	r = newReview("t", target, "title", old, old+"more\n", DefaultTheme, nil)
	_, h = r.Dimensions()
	assert.Equal(t, 101+footerRows, h, "every line of the diff, unfolded")

	r = newReview("t", target, "title", "", "\t\t\tx\n", DefaultTheme, nil)
	assert.Equal(t, 13, r.textWidth, "tabs take TabSpaces cells")
}

func TestReviewButtons(t *testing.T) {
	f := newFixture(t)
	res, r := f.openDiff(t, context.Background())
	r.Resize(66, 4)
	mouse := func(key term.Key, x, y int) (bool, bool) {
		return r.Handle(term.Event{Type: term.EventMouse, Key: key, MouseX: x, MouseY: y})
	}

	// The pointer selects the button under it, and only that one.
	_, handled := mouse(0, 50, 3)
	assert.True(t, handled)
	s := f.draw(r)
	assert.Equal(t, DefaultTheme.Selected.Bg, s.at(48, 3).Bg)
	assert.Equal(t, DefaultTheme.Button.Bg, s.at(37, 3).Bg)
	mouse(0, 50, 99)
	assert.Equal(t, DefaultTheme.Selected.Bg, f.draw(r).at(48, 3).Bg, "the selection stays when it leaves")
	mouse(0, 20, 3)
	assert.Equal(t, 1, r.selected, "and when it moves off the buttons")

	// Dragging off a button does not press it.
	mouse(term.MouseLeft, 50, 3)
	mouse(term.MouseLeft, 20, 3)
	exit, _ := mouse(term.MouseRelease, 20, 3)
	assert.False(t, exit)
	// Nor does releasing on one the mouse did not go down on.
	mouse(term.MouseLeft, 20, 1)
	exit, _ = mouse(term.MouseRelease, 40, 3)
	assert.False(t, exit)

	mouse(term.MouseLeft, 40, 3)
	exit, handled = mouse(term.MouseRelease, 40, 3)
	assert.True(t, exit && handled)
	assert.Equal(t, []string{ideserver.DiffTabClosed}, texts(t, <-res))
	f.fs.assertUntouched(t)
}

func TestReviewSelect(t *testing.T) {
	f := newFixture(t)
	res, r := f.openDiff(t, context.Background())
	r.Resize(66, 4)
	key := func(ev term.Event) bool {
		t.Helper()
		ev.Type = term.EventKey
		exit, handled := r.Handle(ev)
		assert.False(t, exit)
		return handled
	}
	ctrl := func(ch rune) term.Event { return term.Event{Ch: ch, Mod: term.ModCtrl} }

	assert.Equal(t, 0, r.selected, "accept is selected first")
	assert.False(t, key(term.Event{Key: term.KeyArrowLeft}), "the first button leaves Left to Rune")
	assert.True(t, key(term.Event{Key: term.KeyArrowRight}))
	assert.Equal(t, 1, r.selected)
	assert.True(t, key(ctrl('l')))
	assert.Equal(t, 2, r.selected)
	assert.False(t, key(ctrl('f')), "the last button leaves Ctrl-f to Rune")
	assert.Equal(t, 2, r.selected)
	assert.True(t, key(ctrl('b')))
	assert.True(t, key(ctrl('h')))
	assert.Equal(t, 0, r.selected)
	assert.True(t, key(ctrl('f')))
	s := f.draw(r)
	assert.Equal(t, DefaultTheme.Button.Bg, s.at(37, 3).Bg)
	assert.Equal(t, DefaultTheme.Selected.Bg, s.at(48, 3).Bg)
	assert.Equal(t, DefaultTheme.Button.Bg, s.at(59, 3).Bg)
	assert.False(t, key(ctrl('x')), "other Ctrl keys are Rune's")

	exit, handled := r.Handle(term.Event{Type: term.EventKey, Key: term.KeyEnter})
	assert.True(t, exit && handled, "Enter presses the selected button")
	assert.Equal(t, []string{ideserver.DiffRejected}, texts(t, <-res))
}

func TestReviewRejectButton(t *testing.T) {
	f := newFixture(t)
	res, r := f.openDiff(t, context.Background())
	r.Resize(66, 4)
	r.Handle(term.Event{Type: term.EventMouse, Key: term.MouseLeft, MouseX: 50, MouseY: 3})
	exit, _ := r.Handle(term.Event{Type: term.EventMouse, Key: term.MouseRelease, MouseX: 51, MouseY: 3})
	assert.True(t, exit)
	assert.Equal(t, []string{ideserver.DiffRejected}, texts(t, <-res))
}

func TestReviewHighlight(t *testing.T) {
	f := newFixture(t)
	purple := term.Attributes{Fg: term.ColorPurple, Attrs: term.AttrBold, Bg: term.ColorBlue}
	f.parser.locs = map[string][]textapi.Location{
		"package main\n": {{From: term.Coordinates{}, To: term.Coordinates{X: 7}, Attr: purple}},
		"package main\n// proposed\n": {
			{From: term.Coordinates{}, To: term.Coordinates{X: 7}, Attr: purple},
			// A comment that runs to the end of the text.
			{From: term.Coordinates{Y: 1}, To: term.Coordinates{Y: 2}, Attr: term.Attributes{Fg: term.ColorYellow}},
		},
	}
	_, r := f.openDiff(t, context.Background())
	r.Resize(66, 4)
	s := f.draw(r)
	assert.Equal(t, term.ColorPurple, s.at(4, 0).Fg)
	assert.NotZero(t, s.at(4, 0).Attrs&term.AttrBold)
	assert.False(t, s.at(4, 0).Bg.Valid(), "highlights keep the line's background")
	assert.Equal(t, term.ColorDefault, s.at(11, 0).Fg)
	assert.Equal(t, term.ColorYellow, s.at(4, 1).Fg)
	assert.Equal(t, term.ColorYellow, s.at(14, 1).Fg)
	assert.Equal(t, DefaultTheme.Insert.Bg, s.at(4, 1).Bg)
	assert.Equal(t, []string{"file:///ws/pkg/main.go", "file:///ws/pkg/main.go"}, f.parser.uris())
}

func TestReviewJump(t *testing.T) {
	old := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n15\n16\n17\n18\n19\n20\n"
	proposed := strings.Replace(strings.Replace(old, "\n2\n", "\nTWO\n", 1), "\n18\n", "\nEIGHTEEN\n", 1)
	r := newReview("t", target, "title", old, proposed, DefaultTheme, nil)
	r.Resize(66, 4+footerRows)
	require.Equal(t, []int{1, 14}, r.changes)
	press(t, r, ']', false)
	assert.Equal(t, 11, r.offset)
	press(t, r, ']', false)
	assert.Equal(t, 11, r.offset, "no change below")
	press(t, r, '[', false)
	assert.Equal(t, 0, r.offset)
}

func TestReviewLayout(t *testing.T) {
	old := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n15\n16\n17\n18\n19\n20\n"
	proposed := strings.Replace(strings.Replace(old, "\n2\n", "\nTWO\n", 1), "\n18\n", "\nEIGHTEEN\n", 1)
	r := newReview("t", target, "title", old, proposed, DefaultTheme, nil)
	folds := func() int {
		n := 0
		for _, l := range r.lines {
			if l.kind == lineFold {
				n++
			}
		}
		return n
	}

	r.Resize(66, 22+footerRows)
	assert.Len(t, r.lines, 22, "a window with room for the whole diff folds nothing")
	assert.Zero(t, folds())

	r.Resize(66, 20+footerRows)
	assert.Len(t, r.lines, 20, "a shorter one folds what does not fit")
	assert.Equal(t, 1, folds())

	r.Resize(66, 10+footerRows)
	assert.Len(t, r.lines, 18, "and keeps minContext lines around each change")
	assert.Equal(t, 1, folds())
}

func TestReviewClosedFromBar(t *testing.T) {
	f := newFixture(t)
	res, r := f.openDiff(t, context.Background())
	require.NoError(t, r.Close())
	callTool(t, f.srv, "close_tab", map[string]any{"tab_name": r.name})
	assert.Equal(t, []string{ideserver.DiffTabClosed}, texts(t, <-res))
	assert.Empty(t, f.wm.closedIDs(), "the window is gone already")
}

func TestNewFileTitle(t *testing.T) {
	f := newFixture(t)
	res, r := f.openDiff(t, context.Background(), "/elsewhere/new.go")
	assert.Equal(t, "Claude Code wants to create /elsewhere/new.go", f.wm.lastConfig().Title)
	r.Resize(66, 4)
	assert.True(t, strings.HasPrefix(f.screen(r), "+ 1 package main\n+ 2 // proposed\n"), f.screen(r))
	press(t, r, 'a', true)
	<-res
}

func TestThemeFrom(t *testing.T) {
	cfg := config.MapConfig(map[string]any{
		"editor": map[string]any{
			"mode":      "modeless",
			"tabspaces": 2,
			"standard":  map[string]any{"attr": map[string]any{"bg": "#101010"}},
			"vim":       map[string]any{"attr": map[string]any{"bg": "#202020"}},
			"aux_bar":   map[string]any{"line_number_attr": map[string]any{"fg": "silver"}},
		},
		"browser": map[string]any{
			"prompt": map[string]any{
				"text_attr":      map[string]any{"bg": "navy"},
				"highlight_attr": map[string]any{"bg": "teal", "flag": "bold"},
			},
		},
	})
	got := ThemeFrom(cfg)
	assert.Equal(t, 2, got.TabSpaces)
	assert.Equal(t, term.Attributes{Bg: term.NewHexColor(0x101010)}, got.Text, "the standard editor's")
	assert.Equal(t, term.Attributes{Bg: term.GetColor("navy")}, got.Button)
	assert.Equal(t, term.Attributes{Fg: term.GetColor("silver")}, got.LineNumber)
	assert.Equal(t, term.Attributes{Bg: term.GetColor("teal"), Attrs: term.AttrBold}, got.Selected)
	assert.Equal(t, DefaultTheme.Inserted, got.Inserted, "unset keys keep the defaults")
	assert.Equal(t, DefaultTheme.Insert, got.Insert)

	vim := ThemeFrom(config.MapConfig(map[string]any{
		"editor": map[string]any{"vim": map[string]any{"attr": map[string]any{"bg": "#202020"}}},
	}))
	assert.Equal(t, term.Attributes{Bg: term.NewHexColor(0x202020)}, vim.Text, "Rune's editor is vim by default")

	assert.Equal(t, DefaultTheme, ThemeFrom(config.ErrConfig(os.ErrPermission)))
}

type fixture struct {
	mgr        *Manager
	srv        *server.MCPServer
	wm         *fakeWM
	opener     *fakeOpener
	fs         *memFS
	parser     *fakeParser
	editor     *fakeEditor
	interrupts *atomicCount
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{
		wm:         &fakeWM{floating: make(chan browserapi.Handler, 4), content: make(chan browserapi.Handler, 4)},
		opener:     &fakeOpener{},
		fs:         newMemFS(map[string]string{target: "package main\n"}),
		parser:     &fakeParser{},
		interrupts: &atomicCount{},
	}
	f.editor = &fakeEditor{fs: f.fs}
	f.mgr = New(Deps{
		WindowManager: f.wm,
		Opener:        f.opener,
		FileSystem:    f.fs,
		Interrupter: term.FuncInterrupter(func(context.Context) error {
			f.interrupts.Add()
			return nil
		}),
		Parser:  f.parser,
		Editor:  f.editor,
		Root:    "/ws",
		TempDir: "/tmp",
	})
	f.srv = server.NewMCPServer("test", "0", server.WithToolCapabilities(false))
	f.mgr.Register(f.srv)
	t.Cleanup(f.mgr.Close)
	return f
}

// openDiff calls openDiff in the background and returns its eventual
// result and the review it showed.
func (f *fixture) openDiff(
	t *testing.T, ctx context.Context, path ...string,
) (<-chan mcp.CallToolResult, *review) {
	t.Helper()
	p := target
	if len(path) > 0 {
		p = path[0]
	}
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "openDiff", "arguments": openDiffArgs{
			OldFilePath: p, NewFilePath: p, TabName: "✻ [Claude Code] main.go (abc123) ⧉",
			NewFileContents: "package main\n// proposed\n",
		}},
	})
	require.NoError(t, err)
	res := make(chan mcp.CallToolResult, 1)
	go func() {
		var out mcp.CallToolResult
		if resp, ok := f.srv.HandleMessage(ctx, data).(mcp.JSONRPCResponse); ok {
			b, _ := json.Marshal(resp.Result)
			_ = json.Unmarshal(b, &out)
		}
		res <- out
	}()
	var r *review
	select {
	case h := <-f.wm.floating:
		r = h.(*review)
	case <-time.After(5 * time.Second):
		t.Fatal("diff not shown")
	}
	require.Eventually(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.win != nil
	}, 5*time.Second, time.Millisecond)
	return res, r
}

func (f *fixture) draw(r *review) *screen {
	w := &screen{cells: map[term.Coordinates]term.Cell{}}
	r.Draw(w)
	return w
}

func (f *fixture) screen(r *review) string {
	return f.draw(r).String(r.width, r.height)
}

func press(t *testing.T, r *review, ch rune, wantExit bool) {
	t.Helper()
	exit, handled := r.Handle(term.Event{Type: term.EventKey, Ch: ch})
	assert.True(t, handled)
	assert.Equal(t, wantExit, exit)
}

func callTool(t *testing.T, s *server.MCPServer, name string, args map[string]any) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	require.NoError(t, err)
	_, ok := s.HandleMessage(context.Background(), data).(mcp.JSONRPCResponse)
	require.True(t, ok)
}

func texts(t *testing.T, res mcp.CallToolResult) []string {
	t.Helper()
	var out []string
	for _, c := range res.Content {
		text, ok := c.(mcp.TextContent)
		require.True(t, ok)
		out = append(out, text.Text)
	}
	return out
}

func mustURI(t *testing.T, s string) workspaceapi.URI {
	t.Helper()
	u, err := workspaceapi.ParseURI(s)
	require.NoError(t, err)
	return u
}

type atomicCount struct {
	mu sync.Mutex
	n  int
}

func (c *atomicCount) Add() { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *atomicCount) Load() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

type screen struct {
	cells map[term.Coordinates]term.Cell
}

func (s *screen) at(x, y int) term.Cell { return s.cells[term.Coordinates{X: x, Y: y}] }

func (s *screen) Context() context.Context                          { return context.Background() }
func (s *screen) SetCell(c term.Coordinates, cell term.Cell)        { s.cells[c] = cell }
func (s *screen) UnionAttributes(term.Coordinates, term.Attributes) {}
func (s *screen) DrawImage(term.Image) bool                         { return false }

func (s *screen) String(width, height int) string {
	rows := make([]string, 0, height)
	for y := range height {
		var b strings.Builder
		for x := range width {
			cell, ok := s.cells[term.Coordinates{X: x, Y: y}]
			if !ok {
				cell.Ch = ' '
			}
			b.WriteRune(cell.Ch)
		}
		rows = append(rows, strings.TrimRight(b.String(), " "))
	}
	return strings.TrimRight(strings.Join(rows, "\n"), "\n")
}

type fakeWindow struct{ id uint64 }

func (w fakeWindow) WindowID() uint64 { return w.id }

type fakeWM struct {
	browserapi.WindowManager
	floating chan browserapi.Handler
	content  chan browserapi.Handler

	mu      sync.Mutex
	closed  []uint64
	configs []browserapi.FloatingConfig
	shown   browserapi.Handler
}

func (w *fakeWM) Floating(
	h browserapi.Floating, cfg browserapi.FloatingConfig,
) (browserapi.Window, error) {
	w.mu.Lock()
	w.configs = append(w.configs, cfg)
	w.shown = h
	w.mu.Unlock()
	w.floating <- h
	return fakeWindow{2}, nil
}

func (w *fakeWM) lastConfig() browserapi.FloatingConfig {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.configs[len(w.configs)-1]
}

// SetWindowContent closes what the window showed, as Rune does.
func (w *fakeWM) SetWindowContent(win browserapi.Window, h browserapi.Handler) error {
	if win.WindowID() != 2 {
		panic("unexpected window")
	}
	w.mu.Lock()
	prev := w.shown
	w.shown = h
	w.mu.Unlock()
	if prev != nil {
		_ = prev.Close()
	}
	w.content <- h
	return nil
}

func (w *fakeWM) CloseWindow(win browserapi.Window) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = append(w.closed, win.WindowID())
	return nil
}

func (w *fakeWM) closedIDs() []uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

// editorAPI names the embedded textapi.Editor, whose Editor method
// fakeEditor defines.
type editorAPI = textapi.Editor

// fakeEditor shows the files of fs, and records where the cursor of
// each is put and the locations set in it.
type fakeEditor struct {
	editorAPI
	fs *memFS

	mu   sync.Mutex
	set  map[string][]term.Coordinates
	locs map[string][]textapi.Location
}

type fakeTextHandler struct {
	textapi.Handler
	uri string
}

func (e *fakeEditor) Editor(uri workspaceapi.URI) (textapi.Handler, error) {
	return fakeTextHandler{uri: uri.String()}, nil
}

func (e *fakeEditor) SetCursor(h textapi.Handler, c term.Coordinates) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.set == nil {
		e.set = map[string][]term.Coordinates{}
	}
	uri := h.(fakeTextHandler).uri
	e.set[uri] = append(e.set[uri], c)
	return nil
}

func (e *fakeEditor) cursors(uri string) []term.Coordinates {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.set[uri]
}

type fakeCellView struct {
	e   *fakeEditor
	uri string
}

func (e *fakeEditor) CellView(h textapi.Handler) textapi.CellView {
	return fakeCellView{e: e, uri: h.(fakeTextHandler).uri}
}

// RawCells has no row for the line break the file ends with.
func (v fakeCellView) RawCells() ([][]term.Cell, error) {
	return cells(strings.TrimSuffix(v.e.fs.get(strings.TrimPrefix(v.uri, "file://")), "\n")), nil
}

func (e *fakeEditor) SetLocationList(
	h textapi.Handler, _ textapi.LocationPriority, id string, list textapi.LocationList,
) error {
	var locs []textapi.Location
	for loc, ok := list.Current(); ok; loc, ok = list.Next() {
		locs = append(locs, loc)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.locs == nil {
		e.locs = map[string][]textapi.Location{}
	}
	e.locs[h.(fakeTextHandler).uri+" "+id] = locs
	return nil
}

func (e *fakeEditor) locations(uri, id string) []textapi.Location {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.locs[uri+" "+id]
}

// fakeParser highlights a text with the locations in locs for it.
type fakeParser struct {
	syntaxapi.Parser
	locs map[string][]textapi.Location

	mu    sync.Mutex
	asked []string
}

func (p *fakeParser) Highlight(
	uri workspaceapi.URI, content string,
) (iterator.Iterator[textapi.Location], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, uri.String())
	return iterator.FromSlice(p.locs[content]), nil
}

func (p *fakeParser) uris() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.asked
}

type fakeHandler struct {
	browserapi.Handler
	uri string
}

type fakeOpener struct {
	browserapi.ResourceOpener
	err error

	mu     sync.Mutex
	opened []string
}

func (o *fakeOpener) Open(uri workspaceapi.URI) (browserapi.Handler, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return nil, o.err
	}
	o.opened = append(o.opened, uri.String())
	return fakeHandler{uri: uri.String()}, nil
}

func (o *fakeOpener) last() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opened[len(o.opened)-1]
}

// memFS is an in-memory workspaceapi.FileSystem. Directories exist
// implicitly.
type memFS struct {
	workspaceapi.FileSystem

	mu      sync.Mutex
	files   map[string]string
	dirs    map[string]bool
	initial map[string]string
}

func newMemFS(files map[string]string) *memFS {
	initial := map[string]string{}
	for k, v := range files {
		initial[k] = v
	}
	return &memFS{files: files, dirs: map[string]bool{}, initial: initial}
}

func (m *memFS) URI(path string) (workspaceapi.URI, error) {
	return workspaceapi.ParseURI("file://" + path)
}

func (m *memFS) MkdirAll(path string, _ os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirs[path] = true
	return nil
}

func (m *memFS) Remove(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[path]; ok {
		delete(m.files, path)
		return nil
	}
	if m.dirs[path] {
		delete(m.dirs, path)
		return nil
	}
	return fs.ErrNotExist
}

func (m *memFS) OpenFile(path string, flag int, _ os.FileMode) (workspaceapi.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if flag == os.O_RDONLY {
		s, ok := m.files[path]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return &memFile{r: bytes.NewReader([]byte(s))}, nil
	}
	return &memFile{close: func(b []byte) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.files[path] = string(b)
	}}, nil
}

func (m *memFS) put(path, s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[path] = s
}

func (m *memFS) get(path string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.files[path]
}

func (m *memFS) paths(prefix string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for p := range m.files {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	for p := range m.dirs {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	return out
}

// assertUntouched checks that the files the test started with were not
// written.
func (m *memFS) assertUntouched(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for p, want := range m.initial {
		assert.Equal(t, want, m.files[p], p)
	}
}

type memFile struct {
	workspaceapi.File
	r     *bytes.Reader
	buf   bytes.Buffer
	close func([]byte)
}

func (f *memFile) Read(p []byte) (int, error)  { return f.r.Read(p) }
func (f *memFile) Write(p []byte) (int, error) { return f.buf.Write(p) }
func (f *memFile) Close() error {
	if f.close != nil {
		f.close(f.buf.Bytes())
	}
	return nil
}

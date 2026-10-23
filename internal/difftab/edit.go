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
	"log/slog"
	"strings"
	"time"

	"github.com/unstablebuild/rune-go-sdk/api/textapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/term"
)

const (
	// loadWait bounds how long Edit waits for Rune to load the proposal
	// into its editor before it moves the cursor and marks the changes,
	// which loading would undo.
	loadWait = 2 * time.Second
	// loadPoll is how often it looks.
	loadPoll = 25 * time.Millisecond
	// remarkDelay is how long after an edit to the proposal its changes
	// are marked again: Rune drops the marks of a buffer on every edit.
	remarkDelay = 150 * time.Millisecond

	// addedID and deletedID name the location lists of the lines
	// inserted and of the places lines were deleted.
	addedID   = "claude-code-added"
	deletedID = "claude-code-deleted"
)

// annotate waits for Rune's editor to load r's proposal from uri, then
// puts its cursor on the first change and marks the changes, as Rune
// marks those git sees.
func (m *Manager) annotate(r *review, uri workspaceapi.URI) {
	if m.deps.Editor == nil {
		return
	}
	eh, ok := m.loaded(uri, len(splitLines(r.proposed)))
	if !ok {
		slog.Info("proposal did not load", "path", r.path)
		return
	}
	if err := m.deps.Editor.SetCursor(eh, term.Coordinates{Y: r.firstChange}); err != nil {
		slog.Info("put the cursor on the change", "path", r.path, "error", err)
	}
	m.mark(eh, r.old, r.proposed)
}

// loaded returns the editor of uri once it holds at least rows lines,
// or false after loadWait.
func (m *Manager) loaded(uri workspaceapi.URI, rows int) (textapi.Handler, bool) {
	deadline := time.Now().Add(loadWait)
	for {
		eh, err := m.deps.Editor.Editor(uri)
		if err == nil {
			cells, err := m.deps.Editor.CellView(eh).RawCells()
			if err == nil && len(cells) >= rows {
				return eh, true
			}
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(loadPoll)
	}
}

// remark marks the changes of the proposal r is editing in uri again,
// remarkDelay after the last edit to it.
func (m *Manager) remark(r *review, uri workspaceapi.URI) {
	if m.deps.Editor == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.remark != nil {
		r.remark.Stop()
	}
	r.remark = time.AfterFunc(remarkDelay, func() {
		eh, err := m.deps.Editor.Editor(uri)
		if err != nil {
			slog.Debug("mark proposal", "path", r.path, "error", err)
			return
		}
		cells, err := m.deps.Editor.CellView(eh).RawCells()
		if err != nil {
			slog.Debug("mark proposal", "path", r.path, "error", err)
			return
		}
		m.mark(eh, r.old, cellsText(cells, strings.HasSuffix(r.old, "\n")))
	})
}

// stopRemark stops marking r's proposal once it is answered or closed.
func (r *review) stopRemark() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.remark != nil {
		r.remark.Stop()
	}
}

// mark sets the location lists of eh, which holds text, to where text
// differs from old.
func (m *Manager) mark(eh textapi.Handler, old, text string) {
	added, deleted := changeLocations(diffAll(old, text), m.deps.Theme)
	for _, l := range []struct {
		priority textapi.LocationPriority
		id       string
		locs     []textapi.Location
	}{
		{textapi.LocationPriorityInfo, addedID, added},
		// Over the line a deletion replaced with others.
		{textapi.LocationPriorityWarning, deletedID, deleted},
	} {
		err := m.deps.Editor.SetLocationList(eh, l.priority, l.id, textapi.LocationSlice(l.locs))
		if err != nil {
			slog.Debug("mark proposal", "error", err)
		}
	}
}

// changeLocations returns the locations of the new side of all that
// were inserted, and the first cell of the lines where lines were
// deleted, whose message is what was deleted. Lines deleted at the end
// are marked on the last line.
func changeLocations(all []line, theme Theme) (added, deleted []textapi.Location) {
	for i := 0; i < len(all); {
		if !isChange(all[i]) {
			i++
			continue
		}
		d := i
		for d < len(all) && all[d].kind == lineDelete {
			d++
		}
		n := d
		for n < len(all) && all[n].kind == lineInsert {
			n++
		}
		if n > d {
			added = append(added, textapi.Location{
				From: term.Coordinates{Y: all[d].new - 1},
				To:   term.Coordinates{Y: all[n-1].new},
				Attr: theme.InsertMark,
				Icon: "+",
			})
		}
		if d > i {
			deleted = append(deleted, textapi.Location{
				From:    term.Coordinates{Y: deletedAt(all, i)},
				To:      term.Coordinates{Y: deletedAt(all, i), X: 1},
				Attr:    theme.DeleteMark,
				Icon:    "-",
				Message: deletedMessage(all[i:d]),
			})
		}
		i = n
	}
	return added, deleted
}

// deletedAt is the 0-based line of the new side that the change at
// all[from] starts at: its first inserted line, or the line after it,
// or the last line when it is at the end.
func deletedAt(all []line, from int) int {
	for _, l := range all[from:] {
		if l.new > 0 {
			return l.new - 1
		}
	}
	for i := from - 1; i >= 0; i-- {
		if all[i].new > 0 {
			return all[i].new - 1
		}
	}
	return 0
}

func deletedMessage(lines []line) string {
	first := strings.TrimSpace(lines[0].text)
	if len(lines) == 1 {
		return "Deleted: " + first
	}
	return fmt.Sprintf("Deleted %d lines: %s …", len(lines), first)
}

// cellsText returns the text of an editor's cells, ending in a line
// break when newline is set, as the text it was loaded from did.
func cellsText(cells [][]term.Cell, newline bool) string {
	var b strings.Builder
	for i, row := range cells {
		if i > 0 {
			b.WriteByte('\n')
		}
		for _, c := range row {
			if c.Ch == 0 {
				continue
			}
			b.WriteRune(c.Ch)
			if c.Extra != nil {
				for _, ch := range c.Extra.Combining {
					b.WriteRune(ch)
				}
			}
		}
	}
	text := b.String()
	if newline && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text
}

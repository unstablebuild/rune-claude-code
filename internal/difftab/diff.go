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
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// contextLines is the number of unchanged lines shown around a change.
const contextLines = 3

type lineKind uint8

const (
	lineEqual lineKind = iota
	lineDelete
	lineInsert
	lineHunk
)

// line is one row of a unified diff. Old and New are 1-based line
// numbers, zero when the line is not on that side.
type line struct {
	kind     lineKind
	old, new int
	text     string
}

// diffLines returns the unified diff of oldText and newText, with
// contextLines of context around each change and a lineHunk header
// before each hunk.
func diffLines(oldText, newText string) []line {
	dmp := diffmatchpatch.New()
	a, b, table := dmp.DiffLinesToRunes(oldText, newText)
	diffs := dmp.DiffCharsToLines(dmp.DiffMainRunes(a, b, false), table)

	var all []line
	o, n := 1, 1
	for _, d := range diffs {
		for _, t := range splitLines(d.Text) {
			switch d.Type {
			case diffmatchpatch.DiffEqual:
				all = append(all, line{kind: lineEqual, old: o, new: n, text: t})
				o++
				n++
			case diffmatchpatch.DiffDelete:
				all = append(all, line{kind: lineDelete, old: o, text: t})
				o++
			case diffmatchpatch.DiffInsert:
				all = append(all, line{kind: lineInsert, new: n, text: t})
				n++
			}
		}
	}

	keep := make([]bool, len(all))
	for i, l := range all {
		if l.kind == lineEqual {
			continue
		}
		for j := max(0, i-contextLines); j <= min(len(all)-1, i+contextLines); j++ {
			keep[j] = true
		}
	}
	var out []line
	for i := 0; i < len(all); {
		if !keep[i] {
			i++
			continue
		}
		j := i
		for j < len(all) && keep[j] {
			j++
		}
		out = append(out, hunkHeader(all[i:j]))
		out = append(out, all[i:j]...)
		i = j
	}
	return out
}

// hunkHeader returns the "@@ -l,s +l,s @@" header of hunk.
func hunkHeader(hunk []line) line {
	var oldStart, oldCount, newStart, newCount int
	for _, l := range hunk {
		if l.old > 0 {
			if oldCount == 0 {
				oldStart = l.old
			}
			oldCount++
		}
		if l.new > 0 {
			if newCount == 0 {
				newStart = l.new
			}
			newCount++
		}
	}
	return line{
		kind: lineHunk,
		text: fmt.Sprintf("@@ -%d,%d +%d,%d @@", oldStart, oldCount, newStart, newCount),
	}
}

// splitLines splits text into lines without their line endings.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\n")
	}
	return lines
}

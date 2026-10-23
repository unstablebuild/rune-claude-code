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
	"strings"
	"unicode/utf8"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// minContext is the fewest unchanged lines a review shows around a
// change; it shows more when its window has room for them.
const minContext = 5

// minCommon is the share of a replaced line that must be unchanged for
// the words that changed in it to be marked; below it the two lines are
// simply different.
const minCommon = 0.4

type lineKind uint8

const (
	lineEqual lineKind = iota
	lineDelete
	lineInsert
	// lineFold stands for unchanged lines the diff leaves out.
	lineFold
)

// line is one row of the diff. Old and New are 1-based line numbers,
// zero when the line is not on that side; those of a lineFold are the
// first lines it hides, and folded is how many. Changed are the rune
// ranges of a deleted or inserted line that differ from the line it
// replaces.
type line struct {
	kind     lineKind
	old, new int
	text     string
	folded   int
	changed  []span
}

// span is the rune range [from, to) of a line.
type span struct{ from, to int }

// diffAll returns every line of the diff of oldText and newText, with
// the words that changed in replaced lines marked. It is empty when the
// texts have the same lines.
func diffAll(oldText, newText string) []line {
	dmp := diffmatchpatch.New()
	a, b, table := dmp.DiffLinesToRunes(oldText, newText)
	diffs := dmp.DiffCharsToLines(dmp.DiffMainRunes(a, b, false), table)

	var all []line
	changed := false
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
				changed = true
			case diffmatchpatch.DiffInsert:
				all = append(all, line{kind: lineInsert, new: n, text: t})
				n++
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	markChanges(all)
	return all
}

// fold returns all with each change and context unchanged lines around
// it, and a lineFold for each run of more than one unchanged line left
// out.
func fold(all []line, context int) []line {
	keep := make([]bool, len(all))
	for i, l := range all {
		if l.kind == lineEqual {
			continue
		}
		for j := max(0, i-context); j <= min(len(all)-1, i+context); j++ {
			keep[j] = true
		}
	}
	var out []line
	for i := 0; i < len(all); {
		j := i
		for j < len(all) && keep[j] == keep[i] {
			j++
		}
		// A fold of one line would take the row the line takes.
		if keep[i] || j-i == 1 {
			out = append(out, all[i:j]...)
		} else {
			out = append(out, line{kind: lineFold, old: all[i].old, new: all[i].new, folded: j - i})
		}
		i = j
	}
	return out
}

// markChanges sets the changed spans of each deleted line and of the
// inserted line that replaces it, pairing a run of deletions with the
// run of insertions right after it, line by line.
func markChanges(lines []line) {
	for i := 0; i < len(lines); {
		d := i
		for d < len(lines) && lines[d].kind == lineDelete {
			d++
		}
		n := d
		for n < len(lines) && lines[n].kind == lineInsert {
			n++
		}
		for k := range min(d-i, n-d) {
			lines[i+k].changed, lines[d+k].changed = changedSpans(lines[i+k].text, lines[d+k].text)
		}
		i = max(n, i+1)
	}
}

// changedSpans returns the rune ranges of oldText and of newText that
// differ, or none when too little of them is the same.
func changedSpans(oldText, newText string) (oldSpans, newSpans []span) {
	dmp := diffmatchpatch.New()
	diffs := dmp.DiffCleanupSemantic(dmp.DiffMain(oldText, newText, false))
	var common, o, n int
	for _, d := range diffs {
		size := utf8.RuneCountInString(d.Text)
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			common += size
			o += size
			n += size
		case diffmatchpatch.DiffDelete:
			oldSpans = append(oldSpans, span{o, o + size})
			o += size
		case diffmatchpatch.DiffInsert:
			newSpans = append(newSpans, span{n, n + size})
			n += size
		}
	}
	if float64(common) < minCommon*float64(max(o, n)) {
		return nil, nil
	}
	return oldSpans, newSpans
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

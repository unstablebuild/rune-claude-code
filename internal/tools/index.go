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

package tools

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/unstablebuild/rune-go-sdk/api/syntaxapi"
	"github.com/unstablebuild/rune-go-sdk/iterator"
)

// defaultLimit is the number of completions symbol_complete returns by
// default.
const defaultLimit = 20

// symbolIndex holds the qualified names of the symbols referenced or
// defined in the workspace, for completion.
type symbolIndex struct {
	parser syntaxapi.Parser

	mu      sync.Mutex
	names   []indexedName
	running bool
	dirty   bool
}

type indexedName struct {
	name  string
	lower string
	// member is lower after the last dot.
	member string
}

// refresh reloads the index in the background. A refresh asked for
// while one runs runs once after it.
func (x *symbolIndex) refresh(ctx context.Context) {
	x.mu.Lock()
	if x.running {
		x.dirty = true
		x.mu.Unlock()
		return
	}
	x.running = true
	x.mu.Unlock()
	go func() {
		for {
			names, err := x.load(ctx)
			x.mu.Lock()
			if err != nil {
				slog.Debug("index symbols", "error", err)
			} else {
				x.names = names
			}
			if !x.dirty || ctx.Err() != nil {
				x.running = false
				x.mu.Unlock()
				return
			}
			x.dirty = false
			x.mu.Unlock()
		}
	}()
}

func (x *symbolIndex) load(ctx context.Context) ([]indexedName, error) {
	it, err := x.parser.ListReferencedSymbols(ctx)
	if err != nil {
		return nil, err
	}
	names, err := iterator.ToSlice(ctx, it)
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	names = slices.Compact(names)
	out := make([]indexedName, len(names))
	for i, n := range names {
		lower := strings.ToLower(n)
		out[i] = indexedName{name: n, lower: lower, member: lower}
		if dot := strings.LastIndexByte(lower, '.'); dot >= 0 {
			out[i].member = lower[dot+1:]
		}
	}
	return out, nil
}

// complete returns up to limit names that start with prefix, or whose
// last component does, ignoring case. Names matching prefix exactly
// come first, then those matching it ignoring case, then those whose
// last component matches; shorter names first within each.
func (x *symbolIndex) complete(prefix string, limit int) []string {
	if limit <= 0 {
		limit = defaultLimit
	}
	out := []string{}
	if prefix == "" {
		return out
	}
	lower := strings.ToLower(prefix)
	type match struct {
		rank int
		name string
	}
	var matches []match
	x.mu.Lock()
	for _, n := range x.names {
		switch {
		case strings.HasPrefix(n.name, prefix):
			matches = append(matches, match{0, n.name})
		case strings.HasPrefix(n.lower, lower):
			matches = append(matches, match{1, n.name})
		case strings.HasPrefix(n.member, lower):
			matches = append(matches, match{2, n.name})
		}
	}
	x.mu.Unlock()
	slices.SortFunc(matches, func(a, b match) int {
		return cmp.Or(
			cmp.Compare(a.rank, b.rank),
			cmp.Compare(len(a.name), len(b.name)),
			strings.Compare(a.name, b.name),
		)
	})
	for _, m := range matches[:min(limit, len(matches))] {
		out = append(out, m.name)
	}
	return out
}

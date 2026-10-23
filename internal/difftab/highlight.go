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
	"context"
	"math"

	"github.com/unstablebuild/rune-go-sdk/api/syntaxapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/term"
)

// maxHighlight is the largest text a review asks Rune to highlight:
// Rune parses all of it and sends every location it finds.
const maxHighlight = 1 << 20

// styled is a rune range [from, to) of a line and the attributes
// Rune's highlighter gives it.
type styled struct {
	from, to int
	attr     term.Attributes
}

// highlights are the styled ranges of a text's lines, by 0-based line.
type highlights map[int][]styled

// highlight asks p for the syntax highlighting of text, read as the
// contents of uri.
func highlight(
	ctx context.Context, p syntaxapi.Parser, uri workspaceapi.URI, text string,
) (highlights, error) {
	if p == nil || text == "" || len(text) > maxHighlight {
		return nil, nil
	}
	it, err := p.Highlight(uri, text)
	if err != nil {
		return nil, err
	}
	defer func() { _ = it.Close() }()
	out := highlights{}
	for {
		loc, ok := it.Next(ctx)
		if !ok {
			break
		}
		for y := loc.From.Y; y <= loc.To.Y; y++ {
			from, to := 0, math.MaxInt
			if y == loc.From.Y {
				from = loc.From.X
			}
			if y == loc.To.Y {
				to = loc.To.X
			}
			if from < to {
				out[y] = append(out[y], styled{from: from, to: to, attr: loc.Attr})
			}
		}
	}
	return out, it.Err()
}

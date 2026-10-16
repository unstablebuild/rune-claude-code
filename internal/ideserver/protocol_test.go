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

package ideserver

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func intPtr(i int) *int { return &i }

// TestProtocolShapes pins the JSON Claude Code reads and writes.
func TestProtocolShapes(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want string
	}{
		{
			name: "lock",
			v: Lock{
				PID:              42,
				WorkspaceFolders: []string{"/work"},
				IDEName:          IDEName,
				Transport:        TransportWebSocket,
				AuthToken:        "0123456789abcdef0123456789abcdef",
				RuneSocket:       "/tmp/rune.sock",
			},
			want: `{"pid":42,"workspaceFolders":["/work"],"ideName":"Rune",` +
				`"transport":"ws","authToken":"0123456789abcdef0123456789abcdef",` +
				`"runeSocket":"/tmp/rune.sock"}`,
		},
		{
			name: "selection",
			v: SelectionChanged{
				Selection: &Range{
					Start: Position{Line: 3, Character: 1},
					End:   Position{Line: 5, Character: 0},
				},
				Text:     "x := 1",
				FilePath: "/work/main.go",
			},
			want: `{"selection":{"start":{"line":3,"character":1},` +
				`"end":{"line":5,"character":0}},"text":"x := 1",` +
				`"filePath":"/work/main.go"}`,
		},
		{
			name: "at_mentioned_file",
			v:    AtMentioned{FilePath: "/work/main.go"},
			want: `{"filePath":"/work/main.go"}`,
		},
		{
			name: "at_mentioned_lines",
			v: AtMentioned{
				FilePath: "/work/main.go", LineStart: intPtr(0), LineEnd: intPtr(4),
			},
			want: `{"filePath":"/work/main.go","lineStart":0,"lineEnd":4}`,
		},
		{
			name: "diagnostics",
			v: []DiagnosticFile{{
				URI: FileURI("/work/a b.go"),
				Diagnostics: []Diagnostic{{
					Message:  "undefined: x",
					Severity: SeverityError,
					Range: Range{
						Start: Position{Line: 1, Character: 2},
						End:   Position{Line: 1, Character: 3},
					},
					Source: "compiler",
					Code:   "UndeclaredName",
				}},
			}},
			want: `[{"uri":"file:///work/a b.go","diagnostics":[{"message":"undefined: x",` +
				`"severity":"Error","range":{"start":{"line":1,"character":2},` +
				`"end":{"line":1,"character":3}},"source":"compiler",` +
				`"code":"UndeclaredName"}]}]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.v)
			require.NoError(t, err)
			require.JSONEq(t, tt.want, string(got))
		})
	}
}

func TestProtocolConstants(t *testing.T) {
	require.Equal(t, "X-Claude-Code-Ide-Authorization", AuthHeader)
	require.Equal(t, "mcp", Subprotocol)
	require.Equal(t, "ws", TransportWebSocket)
	require.Equal(t, "selection_changed", MethodSelectionChanged)
	require.Equal(t, "at_mentioned", MethodAtMentioned)
	require.Equal(t, "ide_connected", MethodIDEConnected)
	require.Equal(t, "FILE_SAVED", DiffFileSaved)
	require.Equal(t, "TAB_CLOSED", DiffTabClosed)
	require.Equal(t, "DIFF_REJECTED", DiffRejected)
}

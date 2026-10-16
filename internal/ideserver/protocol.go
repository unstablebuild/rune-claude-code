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

// This file holds every shape of Claude Code's IDE integration protocol
// that Rune speaks. The protocol is internal to Claude Code and was
// derived from its source (utils/ide.ts, services/mcp/client.ts,
// hooks/useDiffInIDE.ts, hooks/useIdeSelection.ts,
// hooks/useIdeAtMentioned.ts, services/diagnosticTracking.ts); the tests
// in protocol_test.go pin the wire format so a change is deliberate.

const (
	// AuthHeader carries the lock file's token on every WebSocket
	// upgrade.
	AuthHeader = "X-Claude-Code-Ide-Authorization"
	// Subprotocol is the WebSocket subprotocol Claude Code requests.
	Subprotocol = "mcp"
	// IDEName is the name Claude Code shows for Rune under /ide.
	IDEName = "Rune"
	// TransportWebSocket is the lock file transport Claude Code dials
	// as ws://127.0.0.1:<port>.
	TransportWebSocket = "ws"
	// IDEPath is the endpoint Claude Code connects to as its "ide" MCP
	// server.
	IDEPath = "/"
	// ToolsPath is the endpoint the rune-claude shim bridges to Claude
	// Code as an ordinary MCP server.
	ToolsPath = "/mcp"
)

// Notification methods.
const (
	// MethodSelectionChanged is sent by the IDE when the user's
	// selection changes.
	MethodSelectionChanged = "selection_changed"
	// MethodAtMentioned is sent by the IDE to insert an @-mention of a
	// file, optionally a line range, into Claude Code's prompt.
	MethodAtMentioned = "at_mentioned"
	// MethodIDEConnected is sent by Claude Code once it connected.
	MethodIDEConnected = "ide_connected"
)

// Results of openDiff, as the first text block of the tool result.
const (
	// DiffFileSaved accepts the file contents in the second text block,
	// which may differ from the proposed ones.
	DiffFileSaved = "FILE_SAVED"
	// DiffTabClosed accepts the proposed contents.
	DiffTabClosed = "TAB_CLOSED"
	// DiffRejected keeps the file as it is.
	DiffRejected = "DIFF_REJECTED"
)

// Diagnostic severities as Claude Code names them.
const (
	SeverityError   = "Error"
	SeverityWarning = "Warning"
	SeverityInfo    = "Info"
	SeverityHint    = "Hint"
)

// Lock is the content of the lock file Claude Code reads from
// ~/.claude/ide/<port>.lock to find an IDE.
type Lock struct {
	PID              int      `json:"pid"`
	WorkspaceFolders []string `json:"workspaceFolders"`
	IDEName          string   `json:"ideName"`
	Transport        string   `json:"transport"`
	AuthToken        string   `json:"authToken"`
	// RuneSocket is the socket of the Rune workspace that wrote the
	// lock file. Claude Code ignores it; rune-claude uses it to pick the
	// workspace whose terminal started Claude Code when workspace roots
	// nest.
	RuneSocket string `json:"runeSocket,omitempty"`
}

// Position is a zero-based line and character.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a span between two positions.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// SelectionChanged are the params of MethodSelectionChanged.
type SelectionChanged struct {
	Selection *Range `json:"selection"`
	Text      string `json:"text"`
	FilePath  string `json:"filePath"`
}

// AtMentioned are the params of MethodAtMentioned. Lines are
// zero-based.
type AtMentioned struct {
	FilePath  string `json:"filePath"`
	LineStart *int   `json:"lineStart,omitempty"`
	LineEnd   *int   `json:"lineEnd,omitempty"`
}

// IDEConnected are the params of MethodIDEConnected.
type IDEConnected struct {
	PID int `json:"pid"`
}

// DiagnosticFile is one entry of the JSON array getDiagnostics
// returns. URI is file:// followed by the unescaped absolute path, as
// Claude Code compares it to its own.
type DiagnosticFile struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Diagnostic is one diagnostic of a DiagnosticFile.
type Diagnostic struct {
	Message  string `json:"message"`
	Severity string `json:"severity"`
	Range    Range  `json:"range"`
	Source   string `json:"source,omitempty"`
	Code     string `json:"code,omitempty"`
}

// FileURI returns the URI Claude Code uses for path: file:// followed
// by the path, unescaped.
func FileURI(path string) string {
	return "file://" + path
}

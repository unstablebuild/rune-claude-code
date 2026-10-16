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

package idetools

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/semanticapi"
	"github.com/unstablebuild/rune-go-sdk/api/textapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/term"

	"github.com/unstablebuild/rune-claude-code/internal/ideserver"
)

func TestGetDiagnosticsFile(t *testing.T) {
	f := newFixture(t)
	f.lsp.reports["file:///ws/main.go"] = []semanticapi.Diagnostic{{
		Range: semanticapi.Range{
			Start: semanticapi.Position{Line: 3, Character: 1},
			End:   semanticapi.Position{Line: 3, Character: 9},
		},
		Severity: semanticapi.DiagnosticSeverityWarning,
		Message:  "unused variable",
		Source:   "compiler",
		Code:     "UnusedVar",
	}, {
		Message:   "boom",
		CodeInt:   42,
		CodeIsInt: true,
	}}

	res := callTool(t, f.ide.Server(), "getDiagnostics",
		map[string]any{"uri": "file:///ws/main.go"})
	require.False(t, res.IsError)
	assert.Len(t, res.Content, 1)
	assert.JSONEq(t, `[{"uri":"file:///ws/main.go","diagnostics":[
		{"message":"unused variable","severity":"Warning",
		 "range":{"start":{"line":3,"character":1},"end":{"line":3,"character":9}},
		 "source":"compiler","code":"UnusedVar"},
		{"message":"boom","severity":"Error",
		 "range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},
		 "code":"42"}]}]`, resultText(t, res))
	assert.Equal(t, [][]string{{"file:///ws/main.go"}}, f.lsp.changedURIs())
}

func TestGetDiagnosticsTracked(t *testing.T) {
	f := newFixture(t)
	f.lsp.reports["file:///ws/a.go"] = nil
	f.lsp.reports["file:///ws/b.go"] = []semanticapi.Diagnostic{{
		Severity: semanticapi.DiagnosticSeverityHint, Message: "hint",
	}}
	s := f.ide.Server()

	// Without a baseline there is nothing to report.
	assert.Equal(t, "[]", resultText(t, callTool(t, s, "getDiagnostics", nil)))

	// Claude Code takes a baseline of each file before editing it; a
	// file without a language server is left out.
	for _, uri := range []string{
		"file:///ws/a.go", "file:///ws/b.go", "file:///ws/README.md",
	} {
		callTool(t, s, "getDiagnostics", map[string]any{"uri": uri})
	}
	assert.Equal(t, "[]", resultText(t, callTool(t, s, "getDiagnostics",
		map[string]any{"uri": "file:///ws/README.md"})))

	assert.JSONEq(t, `[
		{"uri":"file:///ws/a.go","diagnostics":[]},
		{"uri":"file:///ws/b.go","diagnostics":[{"message":"hint","severity":"Hint",
		 "range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}}}]}
	]`, resultText(t, callTool(t, s, "getDiagnostics", map[string]any{})))

	changed := f.lsp.changedURIs()
	assert.Equal(t, []string{
		"file:///ws/a.go", "file:///ws/b.go", "file:///ws/README.md",
	}, changed[len(changed)-1], "every tracked file is re-checked")
}

func TestGetDiagnosticsTrackedBound(t *testing.T) {
	f := newFixture(t)
	for n := range maxTracked + 2 {
		f.ide.track(string(rune('a'+n%26)) + string(rune('a'+n/26)))
	}
	f.ide.track("ab") // already tracked, moves to the end
	assert.Len(t, f.ide.tracked, maxTracked)
	assert.Equal(t, "ab", f.ide.tracked[maxTracked-1])
	assert.Equal(t, "ca", f.ide.tracked[0])
}

func TestGetDiagnosticsUnsupportedURI(t *testing.T) {
	f := newFixture(t)
	res := callTool(t, f.ide.Server(), "getDiagnostics",
		map[string]any{"uri": "_claude_fs_right:/ws/main.go"})
	assert.True(t, res.IsError)
}

func TestOpenFile(t *testing.T) {
	tests := []struct {
		name       string
		args       map[string]any
		wantShown  bool
		wantCursor *term.Coordinates
		wantErr    bool
	}{{
		name:      "frontmost by default",
		args:      map[string]any{"filePath": "/ws/main.go"},
		wantShown: true,
	}, {
		name: "background",
		args: map[string]any{"filePath": "/ws/main.go", "makeFrontmost": false},
	}, {
		name:       "start text",
		args:       map[string]any{"filePath": "file:///ws/main.go", "startText": "func main() {\n\treturn"},
		wantShown:  true,
		wantCursor: &term.Coordinates{X: 0, Y: 2},
	}, {
		name:       "start text mid line",
		args:       map[string]any{"filePath": "/ws/main.go", "startText": "main", "makeFrontmost": false},
		wantCursor: &term.Coordinates{X: 8, Y: 0},
	}, {
		name:      "start text missing",
		args:      map[string]any{"filePath": "/ws/main.go", "startText": "nope"},
		wantShown: true,
		wantErr:   true,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.editor.lines = []string{"package main", "", "func main() {}"}
			res := callTool(t, f.ide.Server(), "openFile", tt.args)
			require.Equal(t, tt.wantErr, res.IsError, resultText(t, res))
			assert.Equal(t, []string{"file:///ws/main.go"}, f.opener.opened)
			if tt.wantShown {
				assert.Equal(t, []browserapi.Handler{f.opener.handler}, f.wm.shown)
			} else {
				assert.Empty(t, f.wm.shown)
			}
			assert.Equal(t, tt.wantCursor, f.editor.cursor)
		})
	}
}

func TestSelectionChanged(t *testing.T) {
	tests := []struct {
		name  string
		ev    textapi.Event
		wantJ string
	}{{
		name: "forward",
		ev: textapi.Event{
			Start: term.Coordinates{X: 2, Y: 1}, End: term.Coordinates{X: 5, Y: 3},
			Content: "selected",
		},
		wantJ: `{"selection":{"start":{"line":1,"character":2},"end":{"line":3,"character":5}},
			"text":"selected","filePath":"/ws/main.go"}`,
	}, {
		name: "backward",
		ev: textapi.Event{
			Start: term.Coordinates{X: 4, Y: 3}, End: term.Coordinates{X: 1, Y: 1},
			Content: "selected",
		},
		wantJ: `{"selection":{"start":{"line":1,"character":1},"end":{"line":3,"character":4}},
			"text":"selected","filePath":"/ws/main.go"}`,
	}, {
		name: "cleared",
		ev: textapi.Event{
			Start: term.Coordinates{X: 9, Y: 9}, End: term.Coordinates{X: 7, Y: 4},
		},
		wantJ: `{"selection":{"start":{"line":4,"character":6},"end":{"line":4,"character":6}},
			"text":"","filePath":"/ws/main.go"}`,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			sess := f.connect(t)
			tt.ev.Type = textapi.EventTypeSelection
			tt.ev.URI = mustURI(t, "file:///ws/main.go")
			assert.False(t, f.ide.Handle(context.Background(), tt.ev))

			n := sess.next(t)
			assert.Equal(t, ideserver.MethodSelectionChanged, n.Method)
			assert.JSONEq(t, tt.wantJ, paramsJSON(t, n))

			res := callTool(t, f.ide.Server(), "getLatestSelection", nil)
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &got))
			assert.Equal(t, true, got["success"])
			assert.Equal(t, tt.ev.Content, got["text"])
		})
	}
}

func TestSelectionIgnoresOtherSchemes(t *testing.T) {
	f := newFixture(t)
	sess := f.connect(t)
	f.ide.Handle(context.Background(), textapi.Event{
		Type:    textapi.EventTypeSelection,
		URI:     mustURI(t, "term://ws/1"),
		Content: "$ ls",
	})
	sess.none(t)
	res := callTool(t, f.ide.Server(), "getCurrentSelection", nil)
	assert.Contains(t, resultText(t, res), `"success":false`)
}

func TestClaudeSend(t *testing.T) {
	main := mustURI(t, "file:///ws/main.go")
	selectLines := func(f *fixture, uri workspaceapi.URI) {
		f.ide.Handle(context.Background(), textapi.Event{
			Type: textapi.EventTypeSelection, URI: uri,
			Start: term.Coordinates{X: 0, Y: 4}, End: term.Coordinates{X: 3, Y: 6},
			Content: "abc",
		})
	}
	tests := []struct {
		name    string
		setup   func(*fixture)
		cmd     textapi.Command
		connect bool
		want    string
		wantErr string
	}{{
		name:    "file",
		cmd:     textapi.Command{Args: []string{"send"}, URI: main},
		connect: true,
		want:    `{"filePath":"/ws/main.go"}`,
	}, {
		name:    "selection",
		setup:   func(f *fixture) { selectLines(f, main) },
		cmd:     textapi.Command{Args: []string{"send"}, URI: main},
		connect: true,
		want:    `{"filePath":"/ws/main.go","lineStart":4,"lineEnd":6}`,
	}, {
		name: "selection in another file",
		setup: func(f *fixture) {
			selectLines(f, mustURI(t, "file:///ws/other.go"))
		},
		cmd:     textapi.Command{Args: []string{"send"}, URI: main},
		connect: true,
		want:    `{"filePath":"/ws/main.go"}`,
	}, {
		name: "cleared selection",
		setup: func(f *fixture) {
			selectLines(f, main)
			f.ide.Handle(context.Background(), textapi.Event{
				Type: textapi.EventTypeSelection, URI: main,
				End: term.Coordinates{X: 1, Y: 6},
			})
		},
		cmd:     textapi.Command{Args: []string{"send"}, URI: main},
		connect: true,
		want:    `{"filePath":"/ws/main.go"}`,
	}, {
		name:    "not connected",
		cmd:     textapi.Command{Args: []string{"send"}, URI: main},
		wantErr: "no Claude Code session",
	}, {
		name:    "no file",
		cmd:     textapi.Command{Args: []string{"send"}},
		connect: true,
		wantErr: "focus a file",
	}, {
		name:    "usage",
		cmd:     textapi.Command{Args: []string{"bogus"}, URI: main},
		connect: true,
		wantErr: "usage",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.setup != nil {
				tt.setup(f)
			}
			var sess *testSession
			if tt.connect {
				sess = f.connect(t)
			}
			err := f.ide.HandleCommand(context.Background(), tt.cmd)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				if sess != nil {
					sess.none(t)
				}
				return
			}
			require.NoError(t, err)
			n := sess.next(t)
			assert.Equal(t, ideserver.MethodAtMentioned, n.Method)
			assert.JSONEq(t, tt.want, paramsJSON(t, n))
		})
	}
}

func TestOpenEditors(t *testing.T) {
	f := newFixture(t)
	for _, ev := range []textapi.Event{
		{Type: textapi.EventTypeOpen, URI: mustURI(t, "file:///ws/b.go")},
		{Type: textapi.EventTypeOpen, URI: mustURI(t, "file:///ws/a.go")},
		{Type: textapi.EventTypeOpen, URI: mustURI(t, "file:///ws/c.go")},
		{Type: textapi.EventTypeClose, URI: mustURI(t, "file:///ws/c.go")},
	} {
		f.ide.Handle(context.Background(), ev)
	}
	res := callTool(t, f.ide.Server(), "getOpenEditors", nil)
	assert.JSONEq(t, `{"tabs":[
		{"uri":"file:///ws/a.go","label":"a.go"},
		{"uri":"file:///ws/b.go","label":"b.go"}]}`, resultText(t, res))
}

func TestWorkspaceFolders(t *testing.T) {
	f := newFixture(t)
	res := callTool(t, f.ide.Server(), "getWorkspaceFolders", nil)
	assert.JSONEq(t, `{"success":true,"rootPath":"/ws",
		"folders":[{"name":"ws","uri":"file:///ws","path":"/ws"}]}`, resultText(t, res))
}

func TestUnsupported(t *testing.T) {
	f := newFixture(t)
	for _, tool := range []string{"checkDocumentDirty", "saveDocument"} {
		res := callTool(t, f.ide.Server(), tool, map[string]any{"filePath": "/ws/main.go"})
		assert.Contains(t, resultText(t, res), `"success":false`, tool)
	}
}

func TestToolsAdvertised(t *testing.T) {
	f := newFixture(t)
	var res struct {
		Capabilities struct {
			Tools any `json:"tools"`
		} `json:"capabilities"`
	}
	require.NoError(t, json.Unmarshal(handle(t, f.ide.Server(), "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"clientInfo":      map[string]any{"name": "claude-code", "version": "2.1.300"},
		"capabilities":    map[string]any{},
	}), &res))
	assert.NotNil(t, res.Capabilities.Tools)
}

// fixture wires an IDE to fakes.
type fixture struct {
	ide    *IDE
	lsp    *fakeLSP
	editor *fakeEditor
	opener *fakeOpener
	wm     *fakeWM
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{
		lsp:    &fakeLSP{reports: map[string][]semanticapi.Diagnostic{}},
		editor: &fakeEditor{},
		opener: &fakeOpener{handler: &fakeHandler{}},
		wm:     &fakeWM{},
	}
	f.ide = New(Deps{
		Editor:        f.editor,
		LSP:           f.lsp,
		Opener:        f.opener,
		WindowManager: f.wm,
		FileSystem:    fakeFS{},
		Root:          mustURI(t, "file:///ws"),
	})
	return f
}

// connect registers an initialized client session, as ideserver does
// for each connection.
func (f *fixture) connect(t *testing.T) *testSession {
	s := &testSession{ch: make(chan mcp.JSONRPCNotification, 8)}
	require.NoError(t, f.ide.Server().RegisterSession(context.Background(), s))
	t.Cleanup(func() {
		f.ide.Server().UnregisterSession(context.Background(), s.SessionID())
	})
	return s
}

type testSession struct {
	ch chan mcp.JSONRPCNotification
}

func (s *testSession) SessionID() string { return "test" }
func (s *testSession) Initialize()       {}
func (s *testSession) Initialized() bool { return true }
func (s *testSession) NotificationChannel() chan<- mcp.JSONRPCNotification {
	return s.ch
}

func (s *testSession) next(t *testing.T) mcp.JSONRPCNotification {
	t.Helper()
	select {
	case n := <-s.ch:
		return n
	default:
		t.Fatal("no notification")
		return mcp.JSONRPCNotification{}
	}
}

func (s *testSession) none(t *testing.T) {
	t.Helper()
	select {
	case n := <-s.ch:
		t.Fatalf("unexpected notification %s", n.Method)
	default:
	}
}

func paramsJSON(t *testing.T, n mcp.JSONRPCNotification) string {
	t.Helper()
	b, err := json.Marshal(n.Params)
	require.NoError(t, err)
	return string(b)
}

func handle(t *testing.T, s *server.MCPServer, method string, params any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	require.NoError(t, err)
	resp, ok := s.HandleMessage(context.Background(), data).(mcp.JSONRPCResponse)
	require.True(t, ok)
	out, err := json.Marshal(resp.Result)
	require.NoError(t, err)
	return out
}

func callTool(t *testing.T, s *server.MCPServer, name string, args map[string]any) mcp.CallToolResult {
	t.Helper()
	var res mcp.CallToolResult
	require.NoError(t, json.Unmarshal(handle(t, s, "tools/call", map[string]any{
		"name": name, "arguments": args,
	}), &res))
	return res
}

func resultText(t *testing.T, res mcp.CallToolResult) string {
	t.Helper()
	require.NotEmpty(t, res.Content)
	text, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected TextContent, got %T", res.Content[0])
	return text.Text
}

func mustURI(t *testing.T, s string) workspaceapi.URI {
	t.Helper()
	u, err := workspaceapi.ParseURI(s)
	require.NoError(t, err)
	return u
}

// fakeLSP serves diagnostics from reports; a URI without a report has
// no language server.
type fakeLSP struct {
	semanticapi.LSP

	mu      sync.Mutex
	reports map[string][]semanticapi.Diagnostic
	changed []semanticapi.DidChangeWatchedFilesParams
}

func (l *fakeLSP) Diagnostic(
	_ context.Context, p semanticapi.DocumentDiagnosticParams,
) (semanticapi.DocumentDiagnosticReport, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	items, ok := l.reports[p.TextDocument.URI]
	if !ok {
		return semanticapi.DocumentDiagnosticReport{}, errors.New("no server")
	}
	return semanticapi.DocumentDiagnosticReport{Kind: "full", Items: items}, nil
}

func (l *fakeLSP) DidChangeWatchedFiles(
	_ context.Context, p semanticapi.DidChangeWatchedFilesParams,
) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.changed = append(l.changed, p)
	return nil
}

func (l *fakeLSP) changedURIs() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out [][]string
	for _, p := range l.changed {
		var uris []string
		for _, c := range p.Changes {
			uris = append(uris, c.URI)
		}
		out = append(out, uris)
	}
	return out
}

// editorAPI lets fakeEditor embed textapi.Editor, whose Editor method
// would otherwise clash with the field name.
type editorAPI = textapi.Editor

type fakeEditor struct {
	editorAPI
	lines  []string
	cursor *term.Coordinates
}

func (e *fakeEditor) Editor(workspaceapi.URI) (textapi.Handler, error) {
	return &fakeHandler{}, nil
}

func (e *fakeEditor) CellView(textapi.Handler) textapi.CellView {
	return fakeView{lines: e.lines}
}

func (e *fakeEditor) SetCursor(_ textapi.Handler, c term.Coordinates) error {
	e.cursor = &c
	return nil
}

type fakeView struct{ lines []string }

func (v fakeView) RawCells() ([][]term.Cell, error) {
	cells := make([][]term.Cell, len(v.lines))
	for y, l := range v.lines {
		for _, r := range l {
			cells[y] = append(cells[y], term.Cell{Ch: r})
		}
	}
	return cells, nil
}

type fakeHandler struct{ textapi.Handler }

type fakeOpener struct {
	browserapi.ResourceOpener
	handler browserapi.Handler
	opened  []string
}

func (o *fakeOpener) Open(uri workspaceapi.URI) (browserapi.Handler, error) {
	o.opened = append(o.opened, uri.String())
	return o.handler, nil
}

type fakeWindow struct{ browserapi.Window }

type fakeWM struct {
	browserapi.WindowManager
	shown []browserapi.Handler
}

func (w *fakeWM) Focus() (browserapi.Window, error) { return fakeWindow{}, nil }

func (w *fakeWM) SetWindowContent(_ browserapi.Window, h browserapi.Handler) error {
	w.shown = append(w.shown, h)
	return nil
}

type fakeFS struct{ workspaceapi.FileSystem }

func (fakeFS) URI(path string) (workspaceapi.URI, error) {
	return workspaceapi.ParseURI("file://" + path)
}

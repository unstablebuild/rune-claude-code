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
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/semanticapi"
	"github.com/unstablebuild/rune-go-sdk/api/syntaxapi"
	"github.com/unstablebuild/rune-go-sdk/api/textapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/cli/mcptools"
	"github.com/unstablebuild/rune-go-sdk/iterator"
	"github.com/unstablebuild/rune-go-sdk/term"

	"github.com/unstablebuild/rune-claude-code/internal/idetools"
	"github.com/unstablebuild/rune-claude-code/internal/version"
)

func TestToolsListed(t *testing.T) {
	f := newFixture(t)
	var res struct {
		Tools []struct {
			Name string         `json:"name"`
			Meta map[string]any `json:"_meta"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(handle(t, f.tools.Server(), "tools/list", map[string]any{}), &res))
	var names, alwaysLoad []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Meta[mcptools.AlwaysLoadMetaKey] == true {
			alwaysLoad = append(alwaysLoad, tool.Name)
		}
	}
	assert.Len(t, names, 33+7)
	for _, name := range []string{
		"lsp_definition", "lsp_rename", "syntax_search", "rune_open",
		"rune_highlight", "rune_notify", "symbol_complete", "symbol_definition",
		"rune_version", "outline_file",
	} {
		assert.Contains(t, names, name)
	}
	slices.Sort(alwaysLoad)
	assert.Equal(t, []string{
		"lsp_definition", "lsp_diagnostics", "lsp_documentation", "lsp_implementation",
		"lsp_references", "lsp_workspace_symbols", "outline_file",
	}, alwaysLoad)

	var init struct {
		Instructions string `json:"instructions"`
	}
	require.NoError(t, json.Unmarshal(handle(t, f.tools.Server(), "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"clientInfo":      map[string]any{"name": "claude-code", "version": "2.1.300"},
		"capabilities":    map[string]any{},
	}), &init))
	assert.Contains(t, init.Instructions, "instead of grep or rg, whether through Grep or Bash")
	assert.Contains(t, init.Instructions, "pkg.Symbol")
	assert.Contains(t, init.Instructions, "lsp_documentation")
	assert.Contains(t, init.Instructions, "outline_file")
}

func TestRuneOpen(t *testing.T) {
	tests := []struct {
		name       string
		args       map[string]any
		wantShown  int
		wantCursor *term.Coordinates
	}{{
		name: "tab",
		args: map[string]any{"path": "/ws/main.go"},
	}, {
		name:       "line",
		args:       map[string]any{"path": "/ws/main.go", "line": 12},
		wantCursor: &term.Coordinates{Y: 11},
	}, {
		name:      "focus",
		args:      map[string]any{"path": "/ws/main.go", "focus": true},
		wantShown: 1,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			res := callTool(t, f.tools.Server(), "rune_open", tt.args)
			require.False(t, res.IsError, resultText(t, res))
			assert.Equal(t, []string{"file:///ws/main.go"}, f.opener.opened)
			assert.Equal(t, tt.wantShown, f.wm.shown)
			assert.Equal(t, tt.wantCursor, f.editor.cursor)
		})
	}
}

func TestRuneHighlight(t *testing.T) {
	f := newFixture(t)
	res := callTool(t, f.tools.Server(), "rune_highlight", map[string]any{
		"path":  "/ws/main.go",
		"level": "error",
		"ranges": []map[string]any{
			{"startLine": 3, "message": "here"},
			{"startLine": 5, "endLine": 7},
		},
	})
	require.False(t, res.IsError, resultText(t, res))
	assert.Equal(t, "Highlighted 2 ranges in /ws/main.go", resultText(t, res))
	require.Len(t, f.editor.lists, 1)
	got := f.editor.lists[0]
	assert.Equal(t, textapi.LocationPriorityError, got.priority)
	assert.Equal(t, highlightID, got.id)
	attr := term.Attributes{Attrs: term.AttrUnderline, Underline: term.ColorRed}
	assert.Equal(t, []textapi.Location{
		{From: term.Coordinates{Y: 2}, To: term.Coordinates{Y: 3}, Attr: attr, Message: "here"},
		{From: term.Coordinates{Y: 4}, To: term.Coordinates{Y: 7}, Attr: attr},
	}, got.locs)

	res = callTool(t, f.tools.Server(), "rune_highlight", map[string]any{
		"path": "/ws/main.go", "ranges": []any{},
	})
	require.False(t, res.IsError)
	require.Len(t, f.editor.lists, 2)
	assert.Equal(t, textapi.LocationPriorityInfo, f.editor.lists[1].priority)
	assert.Empty(t, f.editor.lists[1].locs)

	res = callTool(t, f.tools.Server(), "rune_highlight", map[string]any{
		"path": "/ws/main.go", "ranges": []map[string]any{{"startLine": 0}},
	})
	assert.True(t, res.IsError)
}

func TestRuneNotify(t *testing.T) {
	f := newFixture(t)
	for level, want := range map[string]browserapi.NotificationLevel{
		"":        browserapi.LevelInfo,
		"success": browserapi.LevelSuccess,
		"warning": browserapi.LevelWarn,
		"error":   browserapi.LevelError,
	} {
		args := map[string]any{"message": "100% done"}
		if level != "" {
			args["level"] = level
		}
		res := callTool(t, f.tools.Server(), "rune_notify", args)
		require.False(t, res.IsError)
		assert.Equal(t, notification{want, "100% done"}, f.notif.last(), level)
	}
}

func TestSymbolComplete(t *testing.T) {
	f := newFixture(t)
	f.parser.setNames("fmt.Println", "fmt.Printf", "http.Handler",
		"http.HandleFunc", "mypkg.Handle", "Handle", "http.Handler")
	f.tools.Handle(context.Background(), textapi.Event{Type: textapi.EventTypeFlush})
	complete := func(prefix string, limit int) []string {
		args := map[string]any{"prefix": prefix}
		if limit > 0 {
			args["limit"] = limit
		}
		var got []string
		res := callTool(t, f.tools.Server(), "symbol_complete", args)
		require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &got))
		return got
	}
	require.Eventually(t, func() bool {
		return len(complete("fmt.", 0)) == 2
	}, 5*time.Second, time.Millisecond)

	assert.Equal(t, []string{"fmt.Printf", "fmt.Println"}, complete("fmt.Pr", 0))
	assert.Equal(t, []string{"http.Handler", "http.HandleFunc"}, complete("http.Han", 0))
	assert.Equal(t, []string{"Handle", "http.Handler", "mypkg.Handle", "http.HandleFunc"},
		complete("handle", 0))
	assert.Equal(t, []string{"Handle", "http.Handler"}, complete("handle", 2))
	assert.Equal(t, []string{}, complete("", 0))
	assert.Equal(t, []string{}, complete("nope", 0))

	f.parser.setNames("fmt.Errorf")
	f.tools.Handle(context.Background(), textapi.Event{Type: textapi.EventTypeFlush})
	require.Eventually(t, func() bool {
		return slices.Equal(complete("fmt.", 0), []string{"fmt.Errorf"})
	}, 5*time.Second, time.Millisecond)
}

// symbol_definition answers where a symbol is defined, which is not the
// site Rune's symbol index finds: that is often a reference.
func TestSymbolDefinition(t *testing.T) {
	const review = "file:///ws/internal/difftab/review.go"
	f := newFixture(t)
	f.parser.matches["term.Writer"] = []syntaxapi.Match{
		{URI: review, Pos: term.Coordinates{X: 7, Y: 41}, Display: "term.Writer"},
		{URI: review, Pos: term.Coordinates{X: 9, Y: 60}, Display: "term.Writer"},
	}
	f.lsp.defs[review] = "file:///mod/rune-go-sdk/term/structures.go"
	define := func(name string) mcp.CallToolResult {
		return callTool(t, f.tools.Server(), "symbol_definition", map[string]any{"name": name})
	}

	res := define("term.Writer")
	require.False(t, res.IsError, resultText(t, res))
	assert.JSONEq(t, `[{"path":"/mod/rune-go-sdk/term/structures.go","line":121,"column":6}]`,
		resultText(t, res), "both references lead to one definition")
	assert.Equal(t, []string{review + ":41:7", review + ":60:9"}, f.lsp.asked)
	assert.Empty(t, f.lsp.searched, "the index answered")
	assert.Len(t, res.Content, 1, "no hint for a qualified name")

	f.lsp.symbols = []semanticapi.SymbolInformation{{
		Name: "Handler", Kind: semanticapi.SymbolKindInterface,
		Location: semanticapi.Location{URI: "file:///ws/h.go", Range: semanticapi.Range{
			Start: semanticapi.Position{Line: 3, Character: 5},
		}},
	}}
	res = define("Handler")
	require.False(t, res.IsError, resultText(t, res))
	assert.JSONEq(t, `[{"path":"/ws/h.go","line":4,"column":6}]`, resultText(t, res),
		"a bare name falls back to the workspace's symbols")
	require.Len(t, res.Content, 2)
	hint, ok := res.Content[1].(mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, hint.Text, `"Handler" has no qualifier`)

	res = define("nope.Missing")
	assert.True(t, res.IsError)
	assert.Contains(t, resultText(t, res), `"nope.Missing" not found`)
}

func TestOutlineFile(t *testing.T) {
	f := newFixture(t)
	sym := func(name string, kind semanticapi.SymbolKind, line uint32,
		children ...semanticapi.DocumentSymbol) semanticapi.DocumentSymbol {
		return semanticapi.DocumentSymbol{Name: name, Kind: kind, Children: children,
			Range: semanticapi.Range{Start: semanticapi.Position{Line: line}}}
	}
	f.lsp.outline = semanticapi.DocumentSymbolResult{DocumentSymbols: []semanticapi.DocumentSymbol{
		sym("Server", semanticapi.SymbolKindStruct, 9,
			sym("addr", semanticapi.SymbolKindField, 10)),
		sym("Server.Start", semanticapi.SymbolKindMethod, 14,
			sym("ln", semanticapi.SymbolKindVariable, 15,
				sym("x", semanticapi.SymbolKindVariable, 16))),
	}}
	outline := func(path string) mcp.CallToolResult {
		return callTool(t, f.tools.Server(), "outline_file", map[string]any{"path": path})
	}

	res := outline("/ws/server.go")
	require.False(t, res.IsError, resultText(t, res))
	assert.Equal(t, "/ws/server.go:10:struct:Server\n"+
		"/ws/server.go:11:field:Server.addr\n"+
		"/ws/server.go:15:method:Server.Start\n"+
		"/ws/server.go:16:variable:Server.Start.ln\n"+
		"/ws/server.go:17:variable:ln.x\n", resultText(t, res))
	assert.Equal(t, []string{"file:///ws/server.go"}, f.lsp.outlined)

	f.lsp.outline = semanticapi.DocumentSymbolResult{SymbolInformation: []semanticapi.SymbolInformation{{
		Name: "main", Kind: semanticapi.SymbolKindFunction,
		Location: semanticapi.Location{URI: "file:///ws/main.go", Range: semanticapi.Range{
			Start: semanticapi.Position{Line: 2},
		}},
	}}}
	assert.Equal(t, "/ws/main.go:3:function:main\n", resultText(t, outline("/ws/main.go")))

	f.lsp.outline = semanticapi.DocumentSymbolResult{}
	assert.Equal(t, "no symbols found", resultText(t, outline("/ws/empty.go")))

	assert.True(t, callTool(t, f.tools.Server(), "outline_file", nil).IsError)
}

func TestRuneVersion(t *testing.T) {
	f := newFixture(t)
	res := callTool(t, f.tools.Server(), "rune_version", nil)
	assert.JSONEq(t, fmt.Sprintf(`{"version":%q,"protocol":%d}`,
		version.Version, version.Protocol), resultText(t, res))
}

type fixture struct {
	tools  *Tools
	lsp    *fakeLSP
	parser *fakeParser
	editor *fakeEditor
	opener *fakeOpener
	wm     *fakeWM
	notif  *fakeNotifications
}

func newFixture(t *testing.T) *fixture {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f := &fixture{
		lsp:    &fakeLSP{defs: map[string]string{}},
		parser: &fakeParser{matches: map[string][]syntaxapi.Match{}},
		editor: &fakeEditor{},
		opener: &fakeOpener{},
		wm:     &fakeWM{},
		notif:  &fakeNotifications{},
	}
	root, err := workspaceapi.ParseURI("file:///ws")
	require.NoError(t, err)
	f.tools = New(ctx, Deps{
		IDE: idetools.Deps{
			Editor:        f.editor,
			LSP:           f.lsp,
			Opener:        f.opener,
			WindowManager: f.wm,
			FileSystem:    fakeFS{},
			Root:          root,
		},
		Parser:        f.parser,
		Notifications: f.notif,
	})
	return f
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

// fakeLSP answers Definition anywhere in a document listed in defs with
// 120:5 of the file defs names, as at a reference to a symbol defined
// there; elsewhere, with the position asked. It records each site asked as
// uri:line:character.
type fakeLSP struct {
	semanticapi.LSP
	defs     map[string]string
	symbols  []semanticapi.SymbolInformation
	outline  semanticapi.DocumentSymbolResult
	asked    []string
	searched []string
	outlined []string
}

func (l *fakeLSP) DocumentSymbol(
	_ context.Context, p semanticapi.DocumentSymbolParams,
) (semanticapi.DocumentSymbolResult, error) {
	l.outlined = append(l.outlined, p.TextDocument.URI)
	return l.outline, nil
}

func (l *fakeLSP) Definition(
	_ context.Context, p semanticapi.DefinitionParams,
) (semanticapi.LocationResult, error) {
	l.asked = append(l.asked, fmt.Sprintf("%s:%d:%d",
		p.TextDocument.URI, p.Position.Line, p.Position.Character))
	uri, pos := p.TextDocument.URI, p.Position
	if def, ok := l.defs[uri]; ok {
		uri, pos = def, semanticapi.Position{Line: 120, Character: 5}
	}
	return semanticapi.LocationResult{Location: &semanticapi.Location{
		URI: uri, Range: semanticapi.Range{Start: pos, End: pos},
	}}, nil
}

func (l *fakeLSP) WorkspaceSymbol(
	_ context.Context, p semanticapi.WorkspaceSymbolParams,
) ([]semanticapi.SymbolInformation, error) {
	l.searched = append(l.searched, p.Query)
	return l.symbols, nil
}

type fakeParser struct {
	syntaxapi.Parser
	matches map[string][]syntaxapi.Match

	mu    sync.Mutex
	names []string
}

func (p *fakeParser) setNames(names ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.names = names
}

func (p *fakeParser) ListReferencedSymbols(context.Context) (iterator.Iterator[string], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return iterator.FromSlice(slices.Clone(p.names)), nil
}

func (p *fakeParser) ResolveSymbol(
	_ context.Context, name string, _ syntaxapi.Progress,
) (iterator.Iterator[syntaxapi.Match], error) {
	if !slices.Contains([]rune(name), '.') {
		return nil, syntaxapi.ErrNoDot
	}
	return iterator.FromSlice(p.matches[name]), nil
}

// editorAPI lets fakeEditor embed textapi.Editor, whose Editor method
// would otherwise clash with the field name.
type editorAPI = textapi.Editor

type locationList struct {
	priority textapi.LocationPriority
	id       string
	locs     []textapi.Location
}

type fakeEditor struct {
	editorAPI
	cursor *term.Coordinates
	lists  []locationList
}

type fakeHandler struct{ textapi.Handler }

func (e *fakeEditor) Editor(workspaceapi.URI) (textapi.Handler, error) {
	return fakeHandler{}, nil
}

func (e *fakeEditor) SetCursor(_ textapi.Handler, c term.Coordinates) error {
	e.cursor = &c
	return nil
}

func (e *fakeEditor) SetLocationList(
	_ textapi.Handler, p textapi.LocationPriority, id string, l textapi.LocationList,
) error {
	var locs []textapi.Location
	for loc, ok := l.Current(); ok; loc, ok = l.Next() {
		locs = append(locs, loc)
	}
	e.lists = append(e.lists, locationList{p, id, locs})
	return nil
}

type fakeOpener struct {
	browserapi.ResourceOpener
	opened []string
}

func (o *fakeOpener) Open(uri workspaceapi.URI) (browserapi.Handler, error) {
	o.opened = append(o.opened, uri.String())
	return fakeHandler{}, nil
}

type fakeWindow struct{ browserapi.Window }

type fakeWM struct {
	browserapi.WindowManager
	shown int
}

func (w *fakeWM) Focus() (browserapi.Window, error) { return fakeWindow{}, nil }

func (w *fakeWM) SetWindowContent(browserapi.Window, browserapi.Handler) error {
	w.shown++
	return nil
}

type fakeFS struct{ workspaceapi.FileSystem }

func (fakeFS) URI(path string) (workspaceapi.URI, error) {
	return workspaceapi.ParseURI("file://" + path)
}

type notification struct {
	level browserapi.NotificationLevel
	msg   string
}

type fakeNotifications struct {
	browserapi.Notifications
	sent []notification
}

func (n *fakeNotifications) Notify(
	level browserapi.NotificationLevel, msg string, args ...any,
) (string, error) {
	n.sent = append(n.sent, notification{level, fmt.Sprintf(msg, args...)})
	return "id", nil
}

func (n *fakeNotifications) last() notification { return n.sent[len(n.sent)-1] }

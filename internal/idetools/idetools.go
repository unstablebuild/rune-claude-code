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

// Package idetools implements the tools and notifications of Claude
// Code's IDE protocol on a Rune workspace: diagnostics from Rune's
// language servers, opening files, and the user's selection and
// @-mentions.
package idetools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/semanticapi"
	"github.com/unstablebuild/rune-go-sdk/api/textapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/iterator"
	"github.com/unstablebuild/rune-go-sdk/term"

	"github.com/unstablebuild/rune-claude-code/internal/ideserver"
	"github.com/unstablebuild/rune-claude-code/internal/version"
)

const (
	// maxTracked bounds the files getDiagnostics re-checks when called
	// without a uri.
	maxTracked = 64
	// diagnosticsTimeout bounds a getDiagnostics call, which Claude Code
	// awaits before each query.
	diagnosticsTimeout = 10 * time.Second
)

// Command is the manual of the claude command, which HandleCommand
// serves.
var Command = textapi.CommandManual{
	Name:     "claude",
	Summary:  "Send the current file and selection to Claude Code.",
	Synopsis: "send",
	Commands: []textapi.CommandManual{{
		Name:    "send",
		Summary: "Mention the current file and selected lines in Claude Code's prompt.",
	}},
}

// Events are the editor events IDE.Handle expects.
var Events = []textapi.EventType{
	textapi.EventTypeSelection,
	textapi.EventTypeOpen,
	textapi.EventTypeClose,
}

// Deps are the workspace services the IDE tools use.
type Deps struct {
	Editor        textapi.Editor
	LSP           semanticapi.LSP
	Opener        browserapi.ResourceOpener
	WindowManager browserapi.WindowManager
	FileSystem    workspaceapi.FileSystem
	// Root is the workspace root, FileSystem.URI(".").
	Root workspaceapi.URI
}

// IDE is the MCP server Claude Code connects to as its IDE. It is also
// the textapi.EventHandler that follows the user's selection and the
// textapi.CommandHandler of Command.
type IDE struct {
	deps    Deps
	srv     *server.MCPServer
	clients atomic.Int64

	mu        sync.Mutex
	selection *ideserver.SelectionChanged
	open      map[string]workspaceapi.URI
	// tracked are the files Claude Code took a diagnostics baseline
	// of, least recent first.
	tracked []string
}

var (
	_ textapi.EventHandler   = (*IDE)(nil)
	_ textapi.CommandHandler = (*IDE)(nil)
)

// New returns an IDE with its tools registered.
func New(deps Deps) *IDE {
	i := &IDE{deps: deps, open: map[string]workspaceapi.URI{}}
	hooks := &server.Hooks{}
	hooks.AddOnRegisterSession(func(context.Context, server.ClientSession) {
		i.clients.Add(1)
	})
	hooks.AddOnUnregisterSession(func(context.Context, server.ClientSession) {
		i.clients.Add(-1)
	})
	i.srv = server.NewMCPServer("rune-ide", version.Version,
		server.WithToolCapabilities(false),
		server.WithHooks(hooks),
	)
	i.srv.AddNotificationHandler(ideserver.MethodIDEConnected,
		func(_ context.Context, n mcp.JSONRPCNotification) {
			slog.Info("claude code connected",
				"pid", n.Params.AdditionalFields["pid"])
		})

	selection := mcp.NewTool("getLatestSelection",
		mcp.WithDescription("Returns the most recent selection in Rune."))
	current := selection
	current.Name = "getCurrentSelection"
	i.srv.AddTools(
		server.ServerTool{Tool: mcp.NewTool("getDiagnostics",
			mcp.WithDescription(
				"Returns language server diagnostics for a file, or for "+
					"every file whose diagnostics were requested before "+
					"when uri is omitted."),
			mcp.WithString("uri",
				mcp.Description("file:// URI of the file.")),
		), Handler: i.getDiagnostics},
		server.ServerTool{Tool: mcp.NewTool("openFile",
			mcp.WithDescription("Opens a file in Rune."),
			mcp.WithString("filePath", mcp.Required(),
				mcp.Description("Absolute path of the file.")),
			mcp.WithBoolean("preview"),
			mcp.WithString("startText",
				mcp.Description("Text to move the cursor to.")),
			mcp.WithString("endText"),
			mcp.WithBoolean("selectToEndOfLine"),
			mcp.WithBoolean("makeFrontmost",
				mcp.Description("Show the file in the focused window. "+
					"Defaults to true.")),
		), Handler: i.openFile},
		server.ServerTool{Tool: current, Handler: i.getSelection},
		server.ServerTool{Tool: selection, Handler: i.getSelection},
		server.ServerTool{Tool: mcp.NewTool("getOpenEditors",
			mcp.WithDescription("Lists the files open in Rune."),
		), Handler: i.getOpenEditors},
		server.ServerTool{Tool: mcp.NewTool("getWorkspaceFolders",
			mcp.WithDescription("Lists the workspace folders."),
		), Handler: i.getWorkspaceFolders},
		server.ServerTool{Tool: mcp.NewTool("checkDocumentDirty",
			mcp.WithDescription("Not supported by Rune."),
			mcp.WithString("filePath"),
		), Handler: unsupported},
		server.ServerTool{Tool: mcp.NewTool("saveDocument",
			mcp.WithDescription("Not supported by Rune."),
			mcp.WithString("filePath"),
		), Handler: unsupported},
	)
	return i
}

// Server returns the MCP server to serve on ideserver.IDEPath.
func (i *IDE) Server() *server.MCPServer { return i.srv }

// Handle follows the user's selection and open files.
func (i *IDE) Handle(_ context.Context, ev textapi.Event) bool {
	if ev.URI.Scheme() != i.deps.Root.Scheme() {
		return false
	}
	switch ev.Type {
	case textapi.EventTypeOpen:
		i.mu.Lock()
		i.open[ev.URI.String()] = ev.URI
		i.mu.Unlock()
	case textapi.EventTypeClose:
		i.mu.Lock()
		delete(i.open, ev.URI.String())
		i.mu.Unlock()
	case textapi.EventTypeSelection:
		sel := selectionOf(ev)
		i.mu.Lock()
		i.selection = &sel
		i.mu.Unlock()
		i.srv.SendNotificationToAllClients(
			ideserver.MethodSelectionChanged, params(sel))
	}
	return false
}

// selectionOf converts a selection event. Rune reports the anchor in
// Start, one past the cursor in End, and an empty Content once the
// selection is cleared, which Claude Code expects as an empty selection
// at the cursor.
func selectionOf(ev textapi.Event) ideserver.SelectionChanged {
	sel := ideserver.SelectionChanged{
		Text:     ev.Content,
		FilePath: ev.URI.Path(),
	}
	if ev.Content == "" {
		cur := ideserver.Position{Line: ev.End.Y, Character: max(ev.End.X-1, 0)}
		sel.Selection = &ideserver.Range{Start: cur, End: cur}
		return sel
	}
	start, end := ev.Start, ev.End
	if start.Y > end.Y || (start.Y == end.Y && start.X > end.X) {
		start, end = end, start
	}
	sel.Selection = &ideserver.Range{
		Start: ideserver.Position{Line: start.Y, Character: start.X},
		End:   ideserver.Position{Line: end.Y, Character: end.X},
	}
	return sel
}

// HandleCommand serves "claude send": it @-mentions the focused file in
// Claude Code's prompt, with the selected lines if any.
func (i *IDE) HandleCommand(_ context.Context, cmd textapi.Command) error {
	if len(cmd.Args) != 1 || cmd.Args[0] != "send" {
		return errors.New("usage: claude send")
	}
	if cmd.URI.Path() == "" || cmd.URI.Scheme() != i.deps.Root.Scheme() {
		return errors.New("focus a file to send it to Claude Code")
	}
	if i.clients.Load() == 0 {
		return errors.New("no Claude Code session is connected to this " +
			"workspace; run claude --ide in a Rune terminal")
	}
	m := ideserver.AtMentioned{FilePath: cmd.URI.Path()}
	i.mu.Lock()
	sel := i.selection
	i.mu.Unlock()
	if sel != nil && sel.FilePath == m.FilePath && sel.Text != "" {
		start, end := sel.Selection.Start.Line, sel.Selection.End.Line
		m.LineStart, m.LineEnd = &start, &end
	}
	i.srv.SendNotificationToAllClients(ideserver.MethodAtMentioned, params(m))
	return nil
}

// Complete implements textapi.CommandHandler. The host completes the
// subcommands from Command.
func (i *IDE) Complete(context.Context, string, []string) (
	iterator.Iterator[string], error,
) {
	return iterator.FromSlice[string](nil), nil
}

func (i *IDE) getDiagnostics(
	ctx context.Context, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	var paths []string
	if uri := req.GetString("uri", ""); uri != "" {
		path, ok := strings.CutPrefix(uri, "file://")
		if !ok {
			return mcp.NewToolResultErrorf("unsupported uri %q", uri), nil
		}
		i.track(path)
		paths = []string{path}
	} else {
		i.mu.Lock()
		paths = slices.Clone(i.tracked)
		i.mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(ctx, diagnosticsTimeout)
	defer cancel()
	return textJSON(i.diagnostics(ctx, paths))
}

// track records that Claude Code holds a diagnostics baseline of path,
// which it compares against getDiagnostics without a uri after editing
// it. Rune pulls diagnostics per file, since language servers rarely
// support workspace-wide pulls.
func (i *IDE) track(path string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.tracked = slices.DeleteFunc(i.tracked, func(p string) bool {
		return p == path
	})
	i.tracked = append(i.tracked, path)
	if n := len(i.tracked); n > maxTracked {
		i.tracked = slices.Delete(i.tracked, 0, n-maxTracked)
	}
}

// diagnostics tells the language servers that paths changed, since
// Claude Code writes them behind Rune's back, then pulls their
// diagnostics. Files whose diagnostics fail, e.g. because no language
// server handles them, are left out.
func (i *IDE) diagnostics(
	ctx context.Context, paths []string,
) []ideserver.DiagnosticFile {
	files := make([]ideserver.DiagnosticFile, 0, len(paths))
	if len(paths) == 0 {
		return files
	}
	changes := make([]semanticapi.FileEvent, len(paths))
	for k, p := range paths {
		changes[k] = semanticapi.FileEvent{
			URI:  ideserver.FileURI(p),
			Type: semanticapi.FileChangeTypeChanged,
		}
	}
	err := i.deps.LSP.DidChangeWatchedFiles(ctx,
		semanticapi.DidChangeWatchedFilesParams{Changes: changes})
	if err != nil {
		slog.Debug("notify watched files", "error", err)
	}

	results := make([]*ideserver.DiagnosticFile, len(paths))
	var wg sync.WaitGroup
	for k, p := range paths {
		wg.Go(func() {
			uri := ideserver.FileURI(p)
			report, err := i.deps.LSP.Diagnostic(ctx,
				semanticapi.DocumentDiagnosticParams{
					TextDocument: semanticapi.TextDocumentIdentifier{URI: uri},
				})
			if err != nil {
				slog.Debug("diagnostics", "path", p, "error", err)
				return
			}
			f := ideserver.DiagnosticFile{
				URI:         uri,
				Diagnostics: make([]ideserver.Diagnostic, len(report.Items)),
			}
			for n, d := range report.Items {
				f.Diagnostics[n] = diagnostic(d)
			}
			results[k] = &f
		})
	}
	wg.Wait()
	for _, f := range results {
		if f != nil {
			files = append(files, *f)
		}
	}
	return files
}

func diagnostic(d semanticapi.Diagnostic) ideserver.Diagnostic {
	code := d.Code
	if d.CodeIsInt {
		code = strconv.Itoa(d.CodeInt)
	}
	return ideserver.Diagnostic{
		Message:  d.Message,
		Severity: severity(d.Severity),
		Range: ideserver.Range{
			Start: ideserver.Position{
				Line:      int(d.Range.Start.Line),
				Character: int(d.Range.Start.Character),
			},
			End: ideserver.Position{
				Line:      int(d.Range.End.Line),
				Character: int(d.Range.End.Character),
			},
		},
		Source: d.Source,
		Code:   code,
	}
}

// severity names s as Claude Code does. A missing severity is an error,
// as LSP clients usually treat it.
func severity(s semanticapi.DiagnosticSeverity) string {
	switch s {
	case semanticapi.DiagnosticSeverityWarning:
		return ideserver.SeverityWarning
	case semanticapi.DiagnosticSeverityInformation:
		return ideserver.SeverityInfo
	case semanticapi.DiagnosticSeverityHint:
		return ideserver.SeverityHint
	default:
		return ideserver.SeverityError
	}
}

func (i *IDE) openFile(
	_ context.Context, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("filePath")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	path = strings.TrimPrefix(path, "file://")
	_, err = Open(i.deps, path, OpenOptions{
		Focus: req.GetBool("makeFrontmost", true),
		Text:  req.GetString("startText", ""),
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("Opened " + path), nil
}

// OpenOptions configure Open.
type OpenOptions struct {
	// Focus shows the file in the focused window.
	Focus bool
	// Line, if positive, is the 1-based line to move the cursor to.
	Line int
	// Text, if set, moves the cursor to the first occurrence of its
	// first line instead.
	Text string
}

// Open opens the file at path in a tab, or finds the tab it is open
// in, and returns its editor.
func Open(d Deps, path string, opts OpenOptions) (textapi.Handler, error) {
	uri, err := d.FileSystem.URI(path)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", path, err)
	}
	h, err := d.Opener.Open(uri)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if opts.Focus {
		win, err := d.WindowManager.Focus()
		if err != nil {
			return nil, fmt.Errorf("focused window: %w", err)
		}
		if err := d.WindowManager.SetWindowContent(win, h); err != nil {
			return nil, fmt.Errorf("show %s: %w", path, err)
		}
	}
	eh, err := d.Editor.Editor(uri)
	if err != nil {
		return nil, fmt.Errorf("editor of %s: %w", path, err)
	}
	switch {
	case opts.Text != "":
		cells, err := d.Editor.CellView(eh).RawCells()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		at, ok := find(cells, opts.Text)
		if !ok {
			return nil, fmt.Errorf("%q not found in %s", opts.Text, path)
		}
		err = d.Editor.SetCursor(eh, at)
		if err != nil {
			return nil, err
		}
	case opts.Line > 0:
		err := d.Editor.SetCursor(eh, term.Coordinates{Y: opts.Line - 1})
		if err != nil {
			return nil, err
		}
	}
	return eh, nil
}

// find returns the position of the first line of text in cells, one
// rune per cell.
func find(cells [][]term.Cell, text string) (term.Coordinates, bool) {
	first, _, _ := strings.Cut(text, "\n")
	needle := []rune(first)
	for y, line := range cells {
		for x := 0; x+len(needle) <= len(line); x++ {
			if matchAt(line[x:], needle) {
				return term.Coordinates{X: x, Y: y}, true
			}
		}
	}
	return term.Coordinates{}, false
}

func matchAt(cells []term.Cell, needle []rune) bool {
	for k, r := range needle {
		if cells[k].Ch != r {
			return false
		}
	}
	return true
}

func (i *IDE) getSelection(
	context.Context, mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	i.mu.Lock()
	sel := i.selection
	i.mu.Unlock()
	if sel == nil {
		return textJSON(map[string]any{
			"success": false, "message": "No selection",
		})
	}
	return textJSON(map[string]any{
		"success":   true,
		"text":      sel.Text,
		"filePath":  sel.FilePath,
		"selection": sel.Selection,
	})
}

type editor struct {
	URI   string `json:"uri"`
	Label string `json:"label"`
}

func (i *IDE) getOpenEditors(
	context.Context, mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	i.mu.Lock()
	tabs := make([]editor, 0, len(i.open))
	for _, u := range i.open {
		tabs = append(tabs, editor{URI: ideserver.FileURI(u.Path()), Label: u.Name()})
	}
	i.mu.Unlock()
	slices.SortFunc(tabs, func(a, b editor) int { return strings.Compare(a.URI, b.URI) })
	return textJSON(map[string]any{"tabs": tabs})
}

func (i *IDE) getWorkspaceFolders(
	context.Context, mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	root := i.deps.Root.Path()
	return textJSON(map[string]any{
		"success": true,
		"folders": []map[string]string{{
			"name": i.deps.Root.Name(),
			"uri":  ideserver.FileURI(root),
			"path": root,
		}},
		"rootPath": root,
	})
}

func unsupported(
	context.Context, mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	return textJSON(map[string]any{
		"success": false,
		"message": "Rune does not share unsaved changes with Claude Code",
	})
}

// textJSON returns v as JSON in a single text block, which is how
// Claude Code reads IDE tool results.
func textJSON(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(b)), nil
}

// params converts a notification's params to the map mcp-go sends.
func params(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		panic(err)
	}
	return m
}

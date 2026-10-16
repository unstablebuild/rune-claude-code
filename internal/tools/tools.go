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

// Package tools is the MCP server the rune-claude helper bridges to
// Claude Code: Rune's language server and syntax tools, plus tools
// that act on the Rune window and complete and resolve symbols for
// the plugin.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/syntaxapi"
	"github.com/unstablebuild/rune-go-sdk/api/textapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/cli/mcptools"
	"github.com/unstablebuild/rune-go-sdk/iterator"
	"github.com/unstablebuild/rune-go-sdk/term"

	"github.com/unstablebuild/rune-claude-code/internal/idetools"
	"github.com/unstablebuild/rune-claude-code/internal/version"
)

// highlightID names the location list rune_highlight sets.
const highlightID = "claude"

const instructions = "These tools run in Rune, the IDE whose terminal this " +
	"session runs in. Prefer the lsp_* tools over Grep for definitions, " +
	"references, implementations, symbols and diagnostics: they ask the " +
	"workspace's language servers. Use rune_open and rune_highlight to " +
	"show the user code in Rune."

// Events are the editor events Tools.Handle expects.
var Events = []textapi.EventType{textapi.EventTypeFlush}

// Deps are the workspace services the tools use.
type Deps struct {
	IDE           idetools.Deps
	Parser        syntaxapi.Parser
	Notifications browserapi.Notifications
}

// Tools is the MCP server served on ideserver.ToolsPath. It is also the
// textapi.EventHandler that keeps the symbol index fresh.
type Tools struct {
	ctx   context.Context
	deps  Deps
	srv   *server.MCPServer
	index *symbolIndex
}

var _ textapi.EventHandler = (*Tools)(nil)

// New returns Tools with every tool registered and starts indexing the
// workspace's symbols. ctx bounds the tools' background work.
func New(ctx context.Context, deps Deps) *Tools {
	t := &Tools{
		ctx:   ctx,
		deps:  deps,
		index: &symbolIndex{parser: deps.Parser},
		srv: server.NewMCPServer("rune", version.Version,
			server.WithToolCapabilities(false),
			server.WithInstructions(instructions),
		),
	}
	mcptools.RegisterLSPTools(ctx, t.srv, deps.IDE.LSP, mcptools.WithAllThemes())
	mcptools.RegisterSyntaxTools(ctx, t.srv, deps.Parser, mcptools.WithAllThemes())
	t.srv.AddTools(
		server.ServerTool{Tool: mcp.NewTool("rune_open",
			mcp.WithDescription("Opens a file in a Rune tab for the user, "+
				"with the cursor at line. Set focus to also show it in the "+
				"user's focused window, in place of what they are looking at."),
			mcp.WithString("path", mcp.Required(),
				mcp.Description("Absolute path of the file.")),
			mcp.WithNumber("line", mcp.Description("1-based line.")),
			mcp.WithBoolean("focus"),
		), Handler: t.open},
		server.ServerTool{Tool: mcp.NewTool("rune_highlight",
			mcp.WithDescription("Highlights line ranges of a file in Rune, "+
				"each with an optional message the user sees at the cursor, "+
				"replacing the previous highlights of that file. An empty "+
				"ranges clears them."),
			mcp.WithString("path", mcp.Required(),
				mcp.Description("Absolute path of the file.")),
			mcp.WithArray("ranges", mcp.Required(), mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"startLine": map[string]any{"type": "number", "description": "1-based first line."},
					"endLine":   map[string]any{"type": "number", "description": "1-based last line, inclusive. Defaults to startLine."},
					"message":   map[string]any{"type": "string"},
				},
				"required": []string{"startLine"},
			})),
			mcp.WithString("level", mcp.Enum("info", "warning", "error"),
				mcp.DefaultString("info")),
		), Handler: t.highlight},
		server.ServerTool{Tool: mcp.NewTool("rune_notify",
			mcp.WithDescription("Shows the user a notification in Rune."),
			mcp.WithString("message", mcp.Required()),
			mcp.WithString("level", mcp.Enum("info", "success", "warning", "error"),
				mcp.DefaultString("info")),
		), Handler: t.notify},
		server.ServerTool{Tool: mcp.NewTool("symbol_complete",
			mcp.WithDescription("Completes a symbol name, as pkg.Symbol, "+
				"from the symbols referenced or defined in the workspace."),
			mcp.WithString("prefix", mcp.Required()),
			mcp.WithNumber("limit", mcp.DefaultNumber(defaultLimit)),
		), Handler: t.complete},
		server.ServerTool{Tool: mcp.NewTool("symbol_resolve",
			mcp.WithDescription("Finds where a qualified symbol, as "+
				"pkg.Symbol, is defined. Returns [{path, line, column, "+
				"display, importPath}], lines and columns 1-based."),
			mcp.WithString("name", mcp.Required()),
		), Handler: t.resolve},
		server.ServerTool{Tool: mcp.NewTool("rune_version",
			mcp.WithDescription("Returns the version of Rune's Claude Code "+
				"extension and of the protocol its plugin speaks."),
		), Handler: t.version},
	)
	t.index.refresh(ctx)
	return t
}

// Server returns the MCP server to serve on ideserver.ToolsPath.
func (t *Tools) Server() *server.MCPServer { return t.srv }

// Handle reindexes symbols when a file is saved.
func (t *Tools) Handle(_ context.Context, ev textapi.Event) bool {
	if ev.Type == textapi.EventTypeFlush {
		t.index.refresh(t.ctx)
	}
	return false
}

func (t *Tools) open(
	_ context.Context, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	_, err = idetools.Open(t.deps.IDE, path, idetools.OpenOptions{
		Focus: req.GetBool("focus", false),
		Line:  req.GetInt("line", 0),
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("Opened " + path), nil
}

type highlightArgs struct {
	Path   string `json:"path"`
	Level  string `json:"level"`
	Ranges []struct {
		StartLine int    `json:"startLine"`
		EndLine   int    `json:"endLine"`
		Message   string `json:"message"`
	} `json:"ranges"`
}

func (t *Tools) highlight(
	_ context.Context, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	var args highlightArgs
	if err := req.BindArguments(&args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	priority, color := textapi.LocationPriorityInfo, term.ColorTeal
	switch args.Level {
	case "warning":
		priority, color = textapi.LocationPriorityWarning, term.ColorOlive
	case "error":
		priority, color = textapi.LocationPriorityError, term.ColorRed
	}
	locs := make([]textapi.Location, len(args.Ranges))
	for i, r := range args.Ranges {
		end := max(r.EndLine, r.StartLine)
		if r.StartLine < 1 {
			return mcp.NewToolResultError("startLine must be at least 1"), nil
		}
		attr := term.Attributes{Attrs: term.AttrUnderline, Underline: color}
		locs[i] = textapi.Location{
			From:    term.Coordinates{Y: r.StartLine - 1},
			To:      term.Coordinates{Y: end},
			Attr:    attr,
			Message: r.Message,
		}
	}
	h, err := idetools.Open(t.deps.IDE, args.Path, idetools.OpenOptions{})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	err = t.deps.IDE.Editor.SetLocationList(h, priority, highlightID,
		textapi.LocationSlice(locs))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Highlighted %d ranges in %s",
		len(locs), args.Path)), nil
}

func (t *Tools) notify(
	_ context.Context, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	msg, err := req.RequireString("message")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	level := browserapi.LevelInfo
	switch req.GetString("level", "info") {
	case "success":
		level = browserapi.LevelSuccess
	case "warning":
		level = browserapi.LevelWarn
	case "error":
		level = browserapi.LevelError
	}
	if _, err := t.deps.Notifications.Notify(level, "%s", msg); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("Notified"), nil
}

func (t *Tools) complete(
	_ context.Context, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	prefix, err := req.RequireString("prefix")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return textJSON(t.index.complete(prefix, req.GetInt("limit", defaultLimit)))
}

// Symbol is a symbol_resolve result.
type Symbol struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Column     int    `json:"column"`
	Display    string `json:"display,omitempty"`
	ImportPath string `json:"importPath,omitempty"`
}

func (t *Tools) resolve(
	ctx context.Context, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	it, err := t.deps.Parser.ResolveSymbol(ctx, name, nil)
	if errors.Is(err, syntaxapi.ErrNoDot) {
		return textJSON([]Symbol{})
	}
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	matches, err := iterator.ToSlice(ctx, it)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	symbols := make([]Symbol, 0, len(matches))
	for _, m := range matches {
		uri, err := workspaceapi.ParseURI(m.URI)
		if err != nil {
			continue
		}
		symbols = append(symbols, Symbol{
			Path:       uri.Path(),
			Line:       m.Pos.Y + 1,
			Column:     m.Pos.X + 1,
			Display:    m.Display,
			ImportPath: m.ImportPath,
		})
	}
	return textJSON(symbols)
}

// VersionInfo is the rune_version result.
type VersionInfo struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
}

func (t *Tools) version(
	context.Context, mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	return textJSON(VersionInfo{Version: version.Version, Protocol: version.Protocol})
}

func textJSON(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(b)), nil
}

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
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/unstablebuild/rune-go-sdk/api/semanticapi"
	"github.com/unstablebuild/rune-go-sdk/cli/mcptools"
)

// outlineDescription is outline_file's, after Rune's agent's tool of that name.
const outlineDescription = `Gets a flat outline of a file: every symbol it ` +
	`defines (functions, types, methods, variables) with its line, kind and name.

Each line is "path:line:kind:name", lines 1-based, for example:
  /ws/extension.go:10:struct:MyType
  /ws/extension.go:12:method:MyType.Handle

Use it to learn a file's structure without reading the whole file.`

// alwaysLoaded marks tool for Claude Code to load up front.
func alwaysLoaded(tool mcp.Tool) mcp.Tool {
	tool.Meta = mcp.NewMetaFromMap(map[string]any{mcptools.AlwaysLoadMetaKey: true})
	return tool
}

func (t *Tools) outline(
	ctx context.Context, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	path, err := req.RequireString("path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	uri, err := t.deps.IDE.FileSystem.URI(path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	res, err := t.deps.IDE.LSP.DocumentSymbol(ctx, semanticapi.DocumentSymbolParams{
		TextDocument: semanticapi.TextDocumentIdentifier{URI: uri.String()},
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var sb strings.Builder
	writeOutline(&sb, path, res.DocumentSymbols, "")
	for _, s := range res.SymbolInformation {
		writeOutlineLine(&sb, path, s.Location.Range.Start.Line, s.Kind, s.Name)
	}
	if sb.Len() == 0 {
		return mcp.NewToolResultText("no symbols found"), nil
	}
	return mcp.NewToolResultText(sb.String()), nil
}

// writeOutline writes a line for each of symbols and, after each, its
// children's, named after their parent.
func writeOutline(
	sb *strings.Builder, path string, symbols []semanticapi.DocumentSymbol, parent string,
) {
	for _, s := range symbols {
		name := s.Name
		if parent != "" {
			name = parent + "." + name
		}
		writeOutlineLine(sb, path, s.Range.Start.Line, s.Kind, name)
		writeOutline(sb, path, s.Children, s.Name)
	}
}

func writeOutlineLine(
	sb *strings.Builder, path string, line uint32, kind semanticapi.SymbolKind, name string,
) {
	fmt.Fprintf(sb, "%s:%d:%s:%s\n", path, line+1,
		strings.ToLower(mcptools.SymbolKindString(kind)), name)
}

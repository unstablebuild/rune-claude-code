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

// Package difftab shows the edits Claude Code proposes as diffs in
// Rune, next to the focused window, and returns the user's decision.
// It never writes the edited file; Claude Code does once the edit is
// accepted.
package difftab

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/textapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/term"

	"github.com/unstablebuild/rune-claude-code/internal/ideserver"
)

// Events are the editor events Manager.Handle expects.
var Events = []textapi.EventType{textapi.EventTypeFlush, textapi.EventTypeClose}

// Deps are the workspace services the diff reviews use.
type Deps struct {
	WindowManager browserapi.WindowManager
	Opener        browserapi.ResourceOpener
	FileSystem    workspaceapi.FileSystem
	Interrupter   term.Interrupter
	// Root is the workspace root path, which titles are relative to.
	Root string
	// TempDir is where proposed contents are written for the user to
	// edit them. It defaults to os.TempDir().
	TempDir string
}

// Manager serves openDiff, close_tab and closeAllDiffTabs. It is also
// the textapi.EventHandler that notices when an edited proposal is
// saved.
type Manager struct {
	deps Deps

	mu      sync.Mutex
	reviews map[string]*review
	// edits are the files holding proposals being edited, by path.
	edits map[string]*review
}

var _ textapi.EventHandler = (*Manager)(nil)

// New returns a Manager.
func New(deps Deps) *Manager {
	if deps.TempDir == "" {
		deps.TempDir = os.TempDir()
	}
	return &Manager{
		deps:    deps,
		reviews: map[string]*review{},
		edits:   map[string]*review{},
	}
}

// Register adds the diff tools to s.
func (m *Manager) Register(s *server.MCPServer) {
	s.AddTools(
		server.ServerTool{Tool: mcp.NewTool("openDiff",
			mcp.WithDescription("Shows a proposed edit and waits for the "+
				"user to accept, reject or edit it."),
			mcp.WithString("old_file_path", mcp.Required()),
			mcp.WithString("new_file_path", mcp.Required()),
			mcp.WithString("new_file_contents", mcp.Required()),
			mcp.WithString("tab_name", mcp.Required()),
		), Handler: m.openDiff},
		server.ServerTool{Tool: mcp.NewTool("close_tab",
			mcp.WithDescription("Closes the diff opened with tab_name."),
			mcp.WithString("tab_name", mcp.Required()),
		), Handler: m.closeTab},
		server.ServerTool{Tool: mcp.NewTool("closeAllDiffTabs",
			mcp.WithDescription("Closes every open diff."),
		), Handler: m.closeAll},
	)
}

type openDiffArgs struct {
	OldFilePath     string `json:"old_file_path"`
	NewFilePath     string `json:"new_file_path"`
	NewFileContents string `json:"new_file_contents"`
	TabName         string `json:"tab_name"`
}

func (m *Manager) openDiff(
	ctx context.Context, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	var args openDiffArgs
	if err := req.BindArguments(&args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if args.NewFilePath == "" || args.TabName == "" {
		return mcp.NewToolResultError("new_file_path and tab_name are required"), nil
	}
	old, err := m.read(args.OldFilePath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	r := newReview(args.TabName, args.NewFilePath, m.title(args.NewFilePath),
		old, args.NewFileContents, m.edit)

	m.mu.Lock()
	prev := m.reviews[r.name]
	m.reviews[r.name] = r
	m.mu.Unlock()
	if prev != nil {
		prev.resolve(ideserver.DiffTabClosed)
	}
	defer m.forget(r)

	if err := m.show(r); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer m.closeWindow(r)

	select {
	case blocks := <-r.done:
		content := make([]mcp.Content, len(blocks))
		for i, b := range blocks {
			content[i] = mcp.NewTextContent(b)
		}
		return &mcp.CallToolResult{Content: content}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// read returns the contents of path, or nothing if it does not exist
// yet.
func (m *Manager) read(path string) (string, error) {
	f, err := m.deps.FileSystem.OpenFile(path, os.O_RDONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(b), nil
}

func (m *Manager) title(path string) string {
	if rel, err := filepath.Rel(m.deps.Root, path); err == nil &&
		m.deps.Root != "" && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// show opens r in a window to the right of the focused one.
func (m *Manager) show(r *review) error {
	focus, err := m.deps.WindowManager.Focus()
	if err != nil {
		return fmt.Errorf("focused window: %w", err)
	}
	win, err := m.deps.WindowManager.Split(browserapi.OrientationRight, focus, r)
	if err != nil {
		return fmt.Errorf("open diff: %w", err)
	}
	r.mu.Lock()
	r.win = win
	r.mu.Unlock()
	return nil
}

// closeWindow closes r's window unless the user already did, which is
// the case once r exited.
func (m *Manager) closeWindow(r *review) {
	r.mu.Lock()
	win, exited := r.win, r.exited
	r.exited = true
	r.mu.Unlock()
	if win == nil || exited {
		return
	}
	if err := m.deps.WindowManager.CloseWindow(win); err != nil {
		slog.Debug("close diff window", "error", err)
	}
}

func (m *Manager) forget(r *review) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reviews[r.name] == r {
		delete(m.reviews, r.name)
	}
}

// closeTab answers the review as accepted, as an IDE does when a diff
// tab is closed; Claude Code closes it once the user answered in the
// terminal and ignores the answer.
func (m *Manager) closeTab(
	_ context.Context, req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("tab_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	m.mu.Lock()
	r := m.reviews[name]
	m.mu.Unlock()
	if r != nil {
		r.resolve(ideserver.DiffTabClosed)
	}
	return mcp.NewToolResultText(ideserver.DiffTabClosed), nil
}

func (m *Manager) closeAll(
	context.Context, mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	reviews := make([]*review, 0, len(m.reviews))
	for _, r := range m.reviews {
		reviews = append(reviews, r)
	}
	m.mu.Unlock()
	for _, r := range reviews {
		r.resolve(ideserver.DiffTabClosed)
	}
	return mcp.NewToolResultText(fmt.Sprintf("Closed %d diff tabs", len(reviews))), nil
}

// edit writes r's proposal to a temporary file and opens it in r's
// window. Saving the file accepts its contents.
func (m *Manager) edit(r *review) {
	if err := m.startEdit(r); err != nil {
		r.editFailed(err)
		if err := m.deps.Interrupter.Interrupt(context.Background()); err != nil {
			slog.Debug("redraw diff", "error", err)
		}
	}
}

func (m *Manager) startEdit(r *review) error {
	var b [8]byte
	_, _ = rand.Read(b[:])
	dir := filepath.Join(m.deps.TempDir, "claude-review-"+hex.EncodeToString(b[:]))
	path := filepath.Join(dir, filepath.Base(r.path))
	if err := m.deps.FileSystem.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := m.write(path, r.proposed); err != nil {
		m.removeEdit(path)
		return err
	}
	uri, err := m.deps.FileSystem.URI(path)
	if err != nil {
		m.removeEdit(path)
		return err
	}
	m.mu.Lock()
	m.edits[uri.Path()] = r
	m.mu.Unlock()
	h, err := m.deps.Opener.Open(uri)
	if err == nil {
		r.mu.Lock()
		win := r.win
		r.mu.Unlock()
		err = m.deps.WindowManager.SetWindowContent(win, h)
	}
	if err != nil {
		m.mu.Lock()
		delete(m.edits, uri.Path())
		m.mu.Unlock()
		m.removeEdit(path)
		return err
	}
	return nil
}

func (m *Manager) write(path, contents string) error {
	f, err := m.deps.FileSystem.OpenFile(path,
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.WriteString(f, contents)
	return errors.Join(err, f.Close())
}

// removeEdit removes the temporary file at path and its directory.
func (m *Manager) removeEdit(path string) {
	for _, p := range []string{path, filepath.Dir(path)} {
		if err := m.deps.FileSystem.Remove(p); err != nil &&
			!errors.Is(err, fs.ErrNotExist) {
			slog.Debug("remove edited proposal", "path", p, "error", err)
		}
	}
}

// Handle accepts an edited proposal when the user saves it, and
// removes it once its buffer is closed.
func (m *Manager) Handle(_ context.Context, ev textapi.Event) bool {
	path := ev.URI.Path()
	m.mu.Lock()
	r, ok := m.edits[path]
	if ok && ev.Type == textapi.EventTypeClose {
		delete(m.edits, path)
	}
	m.mu.Unlock()
	if !ok {
		return false
	}
	switch ev.Type {
	case textapi.EventTypeFlush:
		r.resolve(ideserver.DiffFileSaved, ev.Content)
	case textapi.EventTypeClose:
		m.removeEdit(path)
	}
	return false
}

// Close removes the proposals still being edited.
func (m *Manager) Close() {
	m.mu.Lock()
	paths := make([]string, 0, len(m.edits))
	for p := range m.edits {
		paths = append(paths, p)
	}
	m.edits = map[string]*review{}
	m.mu.Unlock()
	for _, p := range paths {
		m.removeEdit(p)
	}
}

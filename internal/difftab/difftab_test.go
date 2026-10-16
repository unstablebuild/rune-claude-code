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
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/textapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/term"

	"github.com/unstablebuild/rune-claude-code/internal/ideserver"
)

const target = "/ws/pkg/main.go"

func TestDiffLines(t *testing.T) {
	old := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n"
	proposed := "1\nTWO\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n"
	r := &review{gutter: 2}
	var got []string
	for _, l := range diffLines(old, proposed) {
		s, _ := r.format(l)
		got = append(got, s)
	}
	assert.Equal(t, []string{
		" @@ -1,5 +1,5 @@",
		"  1  1   1",
		"  2    - 2",
		"     2 + TWO",
		"  3  3   3",
		"  4  4   4",
		"  5  5   5",
		" @@ -10,3 +10,4 @@",
		" 10 10   10",
		" 11 11   11",
		" 12 12   12",
		"    13 + 13",
	}, got)

	assert.Empty(t, diffLines("same\n", "same\n"))

	newFile := diffLines("", "a\n\nb")
	require.Len(t, newFile, 4)
	assert.Equal(t, "@@ -0,0 +1,3 @@", newFile[0].text)
	assert.Equal(t, line{kind: lineInsert, new: 2, text: ""}, newFile[2])
}

func TestOpenDiffAnswers(t *testing.T) {
	tests := []struct {
		name       string
		answer     func(*testing.T, *fixture, *review)
		want       []string
		wantClosed bool
	}{{
		name:   "accept",
		answer: func(t *testing.T, _ *fixture, r *review) { press(t, r, 'a', true) },
		want:   []string{ideserver.DiffTabClosed},
	}, {
		name:   "reject",
		answer: func(t *testing.T, _ *fixture, r *review) { press(t, r, 'r', true) },
		want:   []string{ideserver.DiffRejected},
	}, {
		name: "close_tab",
		answer: func(t *testing.T, f *fixture, r *review) {
			callTool(t, f.srv, "close_tab", map[string]any{"tab_name": r.name})
		},
		want:       []string{ideserver.DiffTabClosed},
		wantClosed: true,
	}, {
		name: "closeAllDiffTabs",
		answer: func(t *testing.T, f *fixture, _ *review) {
			callTool(t, f.srv, "closeAllDiffTabs", nil)
		},
		want:       []string{ideserver.DiffTabClosed},
		wantClosed: true,
	}, {
		name: "q leaves the answer to the terminal",
		answer: func(t *testing.T, f *fixture, r *review) {
			press(t, r, 'q', true)
			callTool(t, f.srv, "close_tab", map[string]any{"tab_name": r.name})
		},
		want: []string{ideserver.DiffTabClosed},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			res, r := f.openDiff(t, context.Background())
			tt.answer(t, f, r)
			assert.Equal(t, tt.want, texts(t, <-res))
			if tt.wantClosed {
				assert.Equal(t, []uint64{2}, f.wm.closedIDs())
			} else {
				assert.Empty(t, f.wm.closedIDs())
			}
			f.fs.assertUntouched(t)
		})
	}
}

func TestOpenDiffEdit(t *testing.T) {
	f := newFixture(t)
	res, r := f.openDiff(t, context.Background())
	press(t, r, 'e', false)

	var shown browserapi.Handler
	select {
	case shown = <-f.wm.content:
	case <-time.After(5 * time.Second):
		t.Fatal("edit buffer not shown")
	}
	opened := f.opener.last()
	assert.Equal(t, browserapi.Handler(fakeHandler{uri: opened}), shown)
	path, err := workspaceapi.ParseURI(opened)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(path.Path(), "/tmp/claude-review-"), path.Path())
	assert.True(t, strings.HasSuffix(path.Path(), "/main.go"), path.Path())
	assert.Equal(t, "package main\n// proposed\n", f.fs.get(path.Path()))

	// Other buffers are ignored.
	f.mgr.Handle(context.Background(), textapi.Event{
		Type: textapi.EventTypeFlush, URI: mustURI(t, "file://"+target), Content: "x",
	})
	f.mgr.Handle(context.Background(), textapi.Event{
		Type: textapi.EventTypeFlush, URI: path, Content: "package main\n// edited\n",
	})
	assert.Equal(t, []string{ideserver.DiffFileSaved, "package main\n// edited\n"}, texts(t, <-res))
	assert.Equal(t, []uint64{2}, f.wm.closedIDs())

	f.mgr.Handle(context.Background(), textapi.Event{Type: textapi.EventTypeClose, URI: path})
	assert.Empty(t, f.fs.paths("/tmp/"), "the proposal is removed")
	f.fs.assertUntouched(t)
}

func TestOpenDiffEditFails(t *testing.T) {
	f := newFixture(t)
	f.opener.err = os.ErrPermission
	res, r := f.openDiff(t, context.Background())
	r.Resize(80, 10)
	press(t, r, 'e', false)
	require.Eventually(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.status != "" && !r.editing
	}, 5*time.Second, time.Millisecond)
	assert.Contains(t, f.screen(r), "Cannot edit")
	assert.Positive(t, f.interrupts.Load())
	assert.Empty(t, f.fs.paths("/tmp/"), "the proposal is removed")

	press(t, r, 'a', true)
	assert.Equal(t, []string{ideserver.DiffTabClosed}, texts(t, <-res))
}

func TestOpenDiffDisconnect(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	res, _ := f.openDiff(t, ctx)
	cancel()
	got := <-res
	assert.Nil(t, got.Content)
	assert.Equal(t, []uint64{2}, f.wm.closedIDs())
}

func TestOpenDiffSameTabName(t *testing.T) {
	f := newFixture(t)
	first, _ := f.openDiff(t, context.Background())
	second, r := f.openDiff(t, context.Background())
	assert.Equal(t, []string{ideserver.DiffTabClosed}, texts(t, <-first))
	press(t, r, 'r', true)
	assert.Equal(t, []string{ideserver.DiffRejected}, texts(t, <-second))
}

func TestReviewDraw(t *testing.T) {
	f := newFixture(t)
	_, r := f.openDiff(t, context.Background())
	r.Resize(40, 6)
	assert.Equal(t, strings.Join([]string{
		" Claude Code wants to edit pkg/main.go",
		" a accept   r reject   e edit, then save",
		strings.Repeat("─", 40),
		" @@ -1,1 +1,2 @@",
		" 1 1   package main",
		"   2 + // proposed",
	}, "\n"), f.screen(r))

	// Two rows for three lines of diff.
	r.Resize(40, 5)
	press(t, r, 'j', false)
	assert.True(t, strings.HasSuffix(f.screen(r), "─\n 1 1   package main\n   2 + // proposed"))
	press(t, r, 'j', false)
	assert.True(t, strings.HasSuffix(f.screen(r), "─\n 1 1   package main\n   2 + // proposed"))
	press(t, r, 'g', false)
	assert.True(t, strings.HasSuffix(f.screen(r), "─\n @@ -1,1 +1,2 @@\n 1 1   package main"))
	press(t, r, 'G', false)
	assert.True(t, strings.HasSuffix(f.screen(r), "─\n 1 1   package main\n   2 + // proposed"))

	exit, handled := r.Handle(term.Event{Type: term.EventKey, Ch: 'a', Mod: term.ModCtrl})
	assert.False(t, exit || handled, "modified keys are left to Rune")
}

func TestNewFileTitle(t *testing.T) {
	f := newFixture(t)
	res, r := f.openDiff(t, context.Background(), "/elsewhere/new.go")
	r.Resize(60, 5)
	assert.Contains(t, f.screen(r), "wants to edit /elsewhere/new.go")
	assert.Contains(t, f.screen(r), "@@ -0,0 +1,2 @@")
	press(t, r, 'a', true)
	<-res
}

type fixture struct {
	mgr        *Manager
	srv        *server.MCPServer
	wm         *fakeWM
	opener     *fakeOpener
	fs         *memFS
	interrupts *atomicCount
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{
		wm:         &fakeWM{split: make(chan browserapi.Handler, 4), content: make(chan browserapi.Handler, 4)},
		opener:     &fakeOpener{},
		fs:         newMemFS(map[string]string{target: "package main\n"}),
		interrupts: &atomicCount{},
	}
	f.mgr = New(Deps{
		WindowManager: f.wm,
		Opener:        f.opener,
		FileSystem:    f.fs,
		Interrupter: term.FuncInterrupter(func(context.Context) error {
			f.interrupts.Add()
			return nil
		}),
		Root:    "/ws",
		TempDir: "/tmp",
	})
	f.srv = server.NewMCPServer("test", "0", server.WithToolCapabilities(false))
	f.mgr.Register(f.srv)
	t.Cleanup(f.mgr.Close)
	return f
}

// openDiff calls openDiff in the background and returns its eventual
// result and the review it showed.
func (f *fixture) openDiff(
	t *testing.T, ctx context.Context, path ...string,
) (<-chan mcp.CallToolResult, *review) {
	t.Helper()
	p := target
	if len(path) > 0 {
		p = path[0]
	}
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "openDiff", "arguments": openDiffArgs{
			OldFilePath: p, NewFilePath: p, TabName: "✻ [Claude Code] main.go (abc123) ⧉",
			NewFileContents: "package main\n// proposed\n",
		}},
	})
	require.NoError(t, err)
	res := make(chan mcp.CallToolResult, 1)
	go func() {
		var out mcp.CallToolResult
		if resp, ok := f.srv.HandleMessage(ctx, data).(mcp.JSONRPCResponse); ok {
			b, _ := json.Marshal(resp.Result)
			_ = json.Unmarshal(b, &out)
		}
		res <- out
	}()
	var r *review
	select {
	case h := <-f.wm.split:
		r = h.(*review)
	case <-time.After(5 * time.Second):
		t.Fatal("diff not shown")
	}
	require.Eventually(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.win != nil
	}, 5*time.Second, time.Millisecond)
	return res, r
}

func (f *fixture) screen(r *review) string {
	w := &screen{cells: map[term.Coordinates]rune{}}
	r.Draw(w)
	return w.String(r.width, r.height)
}

func press(t *testing.T, r *review, ch rune, wantExit bool) {
	t.Helper()
	exit, handled := r.Handle(term.Event{Type: term.EventKey, Ch: ch})
	assert.True(t, handled)
	assert.Equal(t, wantExit, exit)
}

func callTool(t *testing.T, s *server.MCPServer, name string, args map[string]any) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	require.NoError(t, err)
	_, ok := s.HandleMessage(context.Background(), data).(mcp.JSONRPCResponse)
	require.True(t, ok)
}

func texts(t *testing.T, res mcp.CallToolResult) []string {
	t.Helper()
	var out []string
	for _, c := range res.Content {
		text, ok := c.(mcp.TextContent)
		require.True(t, ok)
		out = append(out, text.Text)
	}
	return out
}

func mustURI(t *testing.T, s string) workspaceapi.URI {
	t.Helper()
	u, err := workspaceapi.ParseURI(s)
	require.NoError(t, err)
	return u
}

type atomicCount struct {
	mu sync.Mutex
	n  int
}

func (c *atomicCount) Add() { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *atomicCount) Load() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

type screen struct {
	cells map[term.Coordinates]rune
}

func (s *screen) Context() context.Context                          { return context.Background() }
func (s *screen) SetCell(c term.Coordinates, cell term.Cell)        { s.cells[c] = cell.Ch }
func (s *screen) UnionAttributes(term.Coordinates, term.Attributes) {}
func (s *screen) DrawImage(term.Image) bool                         { return false }

func (s *screen) String(width, height int) string {
	rows := make([]string, 0, height)
	for y := range height {
		var b strings.Builder
		for x := range width {
			ch, ok := s.cells[term.Coordinates{X: x, Y: y}]
			if !ok {
				ch = ' '
			}
			b.WriteRune(ch)
		}
		rows = append(rows, strings.TrimRight(b.String(), " "))
	}
	return strings.TrimRight(strings.Join(rows, "\n"), "\n")
}

type fakeWindow struct{ id uint64 }

func (w fakeWindow) WindowID() uint64 { return w.id }

type fakeWM struct {
	browserapi.WindowManager
	split   chan browserapi.Handler
	content chan browserapi.Handler

	mu     sync.Mutex
	closed []uint64
}

func (w *fakeWM) Focus() (browserapi.Window, error) { return fakeWindow{1}, nil }

func (w *fakeWM) Split(
	o browserapi.Orientation, win browserapi.Window, h browserapi.Handler,
) (browserapi.Window, error) {
	if o != browserapi.OrientationRight || win.WindowID() != 1 {
		panic("unexpected split")
	}
	w.split <- h
	return fakeWindow{2}, nil
}

func (w *fakeWM) SetWindowContent(win browserapi.Window, h browserapi.Handler) error {
	if win.WindowID() != 2 {
		panic("unexpected window")
	}
	w.content <- h
	return nil
}

func (w *fakeWM) CloseWindow(win browserapi.Window) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = append(w.closed, win.WindowID())
	return nil
}

func (w *fakeWM) closedIDs() []uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

type fakeHandler struct {
	browserapi.Handler
	uri string
}

type fakeOpener struct {
	browserapi.ResourceOpener
	err error

	mu     sync.Mutex
	opened []string
}

func (o *fakeOpener) Open(uri workspaceapi.URI) (browserapi.Handler, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return nil, o.err
	}
	o.opened = append(o.opened, uri.String())
	return fakeHandler{uri: uri.String()}, nil
}

func (o *fakeOpener) last() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opened[len(o.opened)-1]
}

// memFS is an in-memory workspaceapi.FileSystem. Directories exist
// implicitly.
type memFS struct {
	workspaceapi.FileSystem

	mu      sync.Mutex
	files   map[string]string
	dirs    map[string]bool
	initial map[string]string
}

func newMemFS(files map[string]string) *memFS {
	initial := map[string]string{}
	for k, v := range files {
		initial[k] = v
	}
	return &memFS{files: files, dirs: map[string]bool{}, initial: initial}
}

func (m *memFS) URI(path string) (workspaceapi.URI, error) {
	return workspaceapi.ParseURI("file://" + path)
}

func (m *memFS) MkdirAll(path string, _ os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirs[path] = true
	return nil
}

func (m *memFS) Remove(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[path]; ok {
		delete(m.files, path)
		return nil
	}
	if m.dirs[path] {
		delete(m.dirs, path)
		return nil
	}
	return fs.ErrNotExist
}

func (m *memFS) OpenFile(path string, flag int, _ os.FileMode) (workspaceapi.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if flag == os.O_RDONLY {
		s, ok := m.files[path]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return &memFile{r: bytes.NewReader([]byte(s))}, nil
	}
	return &memFile{close: func(b []byte) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.files[path] = string(b)
	}}, nil
}

func (m *memFS) get(path string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.files[path]
}

func (m *memFS) paths(prefix string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for p := range m.files {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	for p := range m.dirs {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	return out
}

// assertUntouched checks that the files the test started with were not
// written.
func (m *memFS) assertUntouched(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for p, want := range m.initial {
		assert.Equal(t, want, m.files[p], p)
	}
}

type memFile struct {
	workspaceapi.File
	r     *bytes.Reader
	buf   bytes.Buffer
	close func([]byte)
}

func (f *memFile) Read(p []byte) (int, error)  { return f.r.Read(p) }
func (f *memFile) Write(p []byte) (int, error) { return f.buf.Write(p) }
func (f *memFile) Close() error {
	if f.close != nil {
		f.close(f.buf.Bytes())
	}
	return nil
}

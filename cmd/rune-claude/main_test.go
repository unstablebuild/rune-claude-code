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

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/unstablebuild/rune-claude-code/internal/ideserver"
)

func TestPick(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "sub"), 0o700))
	link := filepath.Join(tmp, "link")
	require.NoError(t, os.Symlink(real, link))

	lock := func(port int, socket string, folders ...string) ideserver.LockFile {
		return ideserver.LockFile{Port: port, Lock: ideserver.Lock{
			PID: port, WorkspaceFolders: folders, IDEName: ideserver.IDEName,
			Transport: ideserver.TransportWebSocket, RuneSocket: socket,
		}}
	}
	outer := lock(1, "/run/a.sock", "/src")
	inner := lock(2, "/run/b.sock", "/src/proj")
	sibling := lock(3, "/run/c.sock", "/src/project")
	dead := lock(4, "/run/d.sock", "/src/proj/deep")
	vscode := lock(5, "", "/src/proj/deep")
	vscode.IDEName = "Visual Studio Code"
	linked := lock(6, "", link)
	locks := []ideserver.LockFile{outer, inner, sibling, dead, vscode, linked}
	alive := func(pid int) bool { return pid != 4 }

	tests := []struct {
		name   string
		dir    string
		socket string
		want   int
	}{
		{name: "innermost", dir: "/src/proj/deep", want: 2},
		{name: "socket outranks depth", dir: "/src/proj/deep", socket: "/run/a.sock", want: 1},
		{name: "socket elsewhere", dir: "/src/proj", socket: "/run/c.sock", want: 2},
		{name: "root itself", dir: "/src/project", want: 3},
		{name: "prefix is not a parent", dir: "/src/projector", want: 1},
		{name: "outside", dir: "/home", want: 0},
		{name: "symlinked workspace", dir: filepath.Join(real, "sub"), want: 6},
		{name: "symlinked dir", dir: filepath.Join(link, "sub"), want: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := pick(locks, tt.dir, tt.socket, alive)
			assert.Equal(t, tt.want != 0, ok)
			assert.Equal(t, tt.want, got.Port)
		})
	}
}

func TestClaudeEnv(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		port int
		want []string
	}{{
		name: "pins port",
		env:  []string{"HOME=/h", "CLAUDE_CODE_SSE_PORT=1", "CLAUDE_CODE_SSE_PORTX=2"},
		port: 4242,
		want: []string{"HOME=/h", "CLAUDE_CODE_SSE_PORTX=2",
			"CLAUDE_CODE_AUTO_CONNECT_IDE=true", "CLAUDE_CODE_SSE_PORT=4242"},
	}, {
		name: "overrides auto-connect off",
		env:  []string{"CLAUDE_CODE_AUTO_CONNECT_IDE=false", "HOME=/h"},
		port: 4242,
		want: []string{"HOME=/h",
			"CLAUDE_CODE_AUTO_CONNECT_IDE=true", "CLAUDE_CODE_SSE_PORT=4242"},
	}, {
		name: "no workspace keeps port",
		env:  []string{"HOME=/h", "CLAUDE_CODE_SSE_PORT=1"},
		want: []string{"HOME=/h", "CLAUDE_CODE_SSE_PORT=1",
			"CLAUDE_CODE_AUTO_CONNECT_IDE=true"},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, claudeEnv(tt.env, tt.port))
		})
	}
}

func TestServeMCP(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	t.Setenv("RUNE_SOCKET", "")
	ws := t.TempDir()
	t.Chdir(ws)
	cwd, err := os.Getwd()
	require.NoError(t, err)

	tools := server.NewMCPServer("rune", "test", server.WithToolCapabilities(false))
	tools.AddTool(mcp.NewTool("echo", mcp.WithString("text")),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(req.GetString("text", "")), nil
		})
	var connected atomic.Int32
	srv, err := ideserver.Start(context.Background(), ideserver.Config{
		IDE:              server.NewMCPServer("ide", "test"),
		Tools:            tools,
		WorkspaceFolders: []string{cwd},
		LockDir:          filepath.Join(cfgDir, "ide"),
		OnToolsConnected: func() { connected.Add(1) },
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })

	in, stdin := io.Pipe()
	stdout, out := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serveMCP(context.Background(), in, out)
		_ = out.Close()
	}()
	lines := bufio.NewScanner(stdout)
	send := func(msg string) map[string]any {
		t.Helper()
		_, err := io.WriteString(stdin, msg+"\n")
		require.NoError(t, err)
		require.True(t, lines.Scan(), "no response to %s", msg)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(lines.Bytes(), &resp))
		return resp
	}

	resp := send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
		`"protocolVersion":"2025-06-18","capabilities":{},` +
		`"clientInfo":{"name":"claude-code","version":"2.1.300"}}}`)
	assert.Equal(t, "rune", resp["result"].(map[string]any)["serverInfo"].(map[string]any)["name"])
	_, err = io.WriteString(stdin, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n\n")
	require.NoError(t, err)
	resp = send(`{"jsonrpc":"2.0","id":2,"method":"tools/call",` +
		`"params":{"name":"echo","arguments":{"text":"line one\nline two"}}}`)
	content := resp["result"].(map[string]any)["content"].([]any)
	assert.Equal(t, "line one\nline two", content[0].(map[string]any)["text"])
	assert.Equal(t, int32(1), connected.Load())

	require.NoError(t, stdin.Close())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not stop when stdin closed")
	}
}

func TestServeMCPRuneGone(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	ws := t.TempDir()
	t.Chdir(ws)
	cwd, err := os.Getwd()
	require.NoError(t, err)
	connected := make(chan struct{}, 1)
	srv, err := ideserver.Start(context.Background(), ideserver.Config{
		IDE:              server.NewMCPServer("ide", "test"),
		Tools:            server.NewMCPServer("rune", "test"),
		WorkspaceFolders: []string{cwd},
		LockDir:          filepath.Join(cfgDir, "ide"),
		OnToolsConnected: func() { connected <- struct{}{} },
	})
	require.NoError(t, err)

	in, stdin := io.Pipe()
	defer func() { _ = stdin.Close() }()
	done := make(chan error, 1)
	go func() { done <- serveMCP(context.Background(), in, io.Discard) }()
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("not connected")
	}
	require.NoError(t, srv.Close())
	select {
	case err := <-done:
		assert.ErrorContains(t, err, "connection to Rune closed")
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not stop when Rune closed")
	}
}

func TestServeMCPNoWorkspace(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Chdir(t.TempDir())
	in := strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{}}` + "\n")
	var out strings.Builder
	err := serveMCP(context.Background(), in, &out)
	require.ErrorContains(t, err, "pkg install claude-code")

	var resp struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &resp))
	assert.Equal(t, 7, resp.ID)
	assert.Equal(t, -32000, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "pkg install claude-code")
	assert.Equal(t, 1, strings.Count(out.String(), "\n"), "one line")
}

// Claude Code probes with server/discover and only shows the error it
// gets for initialize, so both must be answered.
func TestServeMCPNoWorkspaceAnswersInitializeAfterProbe(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Chdir(t.TempDir())
	in, stdin := io.Pipe()
	defer func() { _ = stdin.Close() }()
	outr, out := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serveMCP(context.Background(), in, out)
		_ = out.Close()
	}()
	replies := bufio.NewScanner(outr)
	reply := func(req string) map[string]any {
		t.Helper()
		_, err := io.WriteString(stdin, req+"\n")
		require.NoError(t, err)
		require.True(t, replies.Scan(), "no reply to %s", req)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(replies.Bytes(), &resp))
		return resp
	}

	probe := reply(`{"jsonrpc":"2.0","id":"server-discover-probe-1","method":"server/discover","params":{}}`)
	assert.Equal(t, "server-discover-probe-1", probe["id"])
	_, err := io.WriteString(stdin, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`+"\n")
	require.NoError(t, err)
	initialize := reply(`{"method":"initialize","params":{},"jsonrpc":"2.0","id":0}`)
	assert.EqualValues(t, 0, initialize["id"])
	assert.Contains(t, initialize["error"].(map[string]any)["message"], "pkg install claude-code")

	select {
	case err := <-done:
		require.ErrorContains(t, err, "pkg install claude-code")
	case <-time.After(5 * time.Second):
		t.Fatal("did not exit after answering initialize")
	}
	assert.False(t, replies.Scan(), "nothing after initialize")
}

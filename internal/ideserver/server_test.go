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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
)

type testEnv struct {
	srv       *Server
	ide       *server.MCPServer
	tools     *server.MCPServer
	dir       string
	connected atomic.Int32
	release   chan struct{}
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	env := &testEnv{
		ide:     server.NewMCPServer("ide", "test"),
		tools:   server.NewMCPServer("tools", "test"),
		dir:     t.TempDir(),
		release: make(chan struct{}),
	}
	env.ide.AddTool(mcp.NewTool("block"), func(
		ctx context.Context, _ mcp.CallToolRequest,
	) (*mcp.CallToolResult, error) {
		select {
		case <-env.release:
			return mcp.NewToolResultText("released"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	env.ide.AddTool(mcp.NewTool("echo"), func(
		_ context.Context, _ mcp.CallToolRequest,
	) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("echo"), nil
	})
	env.tools.AddTool(mcp.NewTool("rune_version"), func(
		_ context.Context, _ mcp.CallToolRequest,
	) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("v"), nil
	})
	srv, err := Start(t.Context(), Config{
		IDE:              env.ide,
		Tools:            env.tools,
		WorkspaceFolders: []string{"/work"},
		LockDir:          env.dir,
		RuneSocket:       "/tmp/rune.sock",
		OnToolsConnected: func() { env.connected.Add(1) },
	})
	require.NoError(t, err)
	env.srv = srv
	t.Cleanup(func() { _ = srv.Close() })
	return env
}

func (e *testEnv) url(path string) string {
	return fmt.Sprintf("ws://127.0.0.1:%d%s", e.srv.Port(), path)
}

func (e *testEnv) lock(t *testing.T) Lock {
	t.Helper()
	data, err := os.ReadFile(e.srv.LockPath())
	require.NoError(t, err)
	var lock Lock
	require.NoError(t, json.Unmarshal(data, &lock))
	return lock
}

func (e *testEnv) dial(t *testing.T, path, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, e.url(path), &websocket.DialOptions{
		HTTPHeader:   http.Header{AuthHeader: []string{token}},
		Subprotocols: []string{Subprotocol},
	})
	require.NoError(t, err)
	require.Equal(t, Subprotocol, resp.Header.Get("Sec-WebSocket-Protocol"))
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func send(t *testing.T, conn *websocket.Conn, msg string) {
	t.Helper()
	require.NoError(t, conn.Write(t.Context(), websocket.MessageText, []byte(msg)))
}

func recv(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	require.NoError(t, err)
	var msg map[string]any
	require.NoError(t, json.Unmarshal(data, &msg))
	return msg
}

const initialize = `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{` +
	`"protocolVersion":"2025-06-18","capabilities":{"roots":{}},` +
	`"clientInfo":{"name":"claude-code","version":"2.1.300"}}}`

func TestLockFile(t *testing.T) {
	env := newTestEnv(t)

	info, err := os.Stat(env.srv.LockPath())
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.Equal(t, LockPath(env.dir, env.srv.Port()), env.srv.LockPath())

	lock := env.lock(t)
	require.Equal(t, os.Getpid(), lock.PID)
	require.Equal(t, []string{"/work"}, lock.WorkspaceFolders)
	require.Equal(t, "Rune", lock.IDEName)
	require.Equal(t, "ws", lock.Transport)
	require.Equal(t, "/tmp/rune.sock", lock.RuneSocket)
	require.Regexp(t, `^[0-9a-f]{32}$`, lock.AuthToken)

	locks, err := ReadLocks(env.dir)
	require.NoError(t, err)
	require.Equal(t, []LockFile{{Port: env.srv.Port(), Lock: lock}}, locks)

	require.NoError(t, env.srv.Close())
	_, err = os.Stat(env.srv.LockPath())
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, env.srv.Close())
}

func TestLockFileRemovedOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	srv, err := Start(ctx, Config{
		IDE:     server.NewMCPServer("ide", "test"),
		Tools:   server.NewMCPServer("tools", "test"),
		LockDir: t.TempDir(),
	})
	require.NoError(t, err)
	cancel()
	require.Eventually(t, func() bool {
		_, err := os.Stat(srv.LockPath())
		return os.IsNotExist(err)
	}, 5*time.Second, 10*time.Millisecond)
}

func TestTokensDiffer(t *testing.T) {
	a, b := newTestEnv(t), newTestEnv(t)
	require.NotEqual(t, a.lock(t).AuthToken, b.lock(t).AuthToken)
}

func TestAuthRejected(t *testing.T) {
	env := newTestEnv(t)
	tests := []struct {
		name   string
		path   string
		header http.Header
	}{
		{name: "ide_missing", path: IDEPath},
		{name: "tools_missing", path: ToolsPath},
		{
			name:   "ide_wrong",
			path:   IDEPath,
			header: http.Header{AuthHeader: []string{"00000000000000000000000000000000"}},
		},
		{
			name:   "tools_wrong",
			path:   ToolsPath,
			header: http.Header{AuthHeader: []string{"nope"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, resp, err := websocket.Dial(t.Context(), env.url(tt.path),
				&websocket.DialOptions{HTTPHeader: tt.header})
			require.Error(t, err)
			require.NotNil(t, resp)
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		})
	}
	require.Zero(t, env.connected.Load())
}

func TestUnknownPath(t *testing.T) {
	env := newTestEnv(t)
	_, resp, err := websocket.Dial(t.Context(), env.url("/other"), &websocket.DialOptions{
		HTTPHeader: http.Header{AuthHeader: []string{env.lock(t).AuthToken}},
	})
	require.Error(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestIDEEndpoint(t *testing.T) {
	env := newTestEnv(t)
	conn := env.dial(t, IDEPath, env.lock(t).AuthToken)

	send(t, conn, initialize)
	resp := recv(t, conn)
	result := resp["result"].(map[string]any)
	require.Contains(t, result["capabilities"], "tools")
	send(t, conn, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(t, conn, `{"jsonrpc":"2.0","method":"ide_connected","params":{"pid":7}}`)

	env.ide.SendNotificationToAllClients(MethodSelectionChanged, map[string]any{
		"text": "x", "filePath": "/work/a.go",
	})
	n := recv(t, conn)
	require.Equal(t, MethodSelectionChanged, n["method"])
	require.Equal(t, map[string]any{"text": "x", "filePath": "/work/a.go"}, n["params"])

	// A blocked tool call does not hold up the next one.
	send(t, conn, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"block"}}`)
	send(t, conn, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo"}}`)
	require.EqualValues(t, 2, recv(t, conn)["id"])
	close(env.release)
	require.EqualValues(t, 1, recv(t, conn)["id"])
	require.Zero(t, env.connected.Load())
}

func TestToolsEndpoint(t *testing.T) {
	env := newTestEnv(t)
	conn := env.dial(t, ToolsPath, env.lock(t).AuthToken)
	require.EqualValues(t, 1, env.connected.Load())

	send(t, conn, initialize)
	recv(t, conn)
	send(t, conn, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := recv(t, conn)["result"].(map[string]any)["tools"].([]any)
	require.Len(t, tools, 1)
	require.Equal(t, "rune_version", tools[0].(map[string]any)["name"])
}

func TestCloseDisconnectsClients(t *testing.T) {
	env := newTestEnv(t)
	conn := env.dial(t, IDEPath, env.lock(t).AuthToken)
	send(t, conn, initialize)
	recv(t, conn)
	// A call blocked in a tool is cancelled rather than holding Close.
	send(t, conn, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"block"}}`)

	done := make(chan error, 1)
	go func() { done <- env.srv.Close() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			require.NoError(t, ctx.Err(), "connection was not closed")
			break
		}
	}
}

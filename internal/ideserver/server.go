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

// Package ideserver makes a Rune workspace discoverable by Claude Code.
//
// It listens on a random loopback port, advertises it with a lock file
// under Claude Code's ide directory, and serves two MCP servers over
// WebSocket: Claude Code's IDE protocol on IDEPath and Rune's tools on
// ToolsPath. Both require the lock file's token.
package ideserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mark3labs/mcp-go/server"
)

// maxMessageSize bounds a single JSON-RPC message. openDiff carries a
// whole file.
const maxMessageSize = 64 << 20

// Config configures a Server.
type Config struct {
	// IDE is served on IDEPath.
	IDE *server.MCPServer
	// Tools is served on ToolsPath.
	Tools *server.MCPServer
	// WorkspaceFolders are the absolute paths Claude Code matches its
	// working directory against.
	WorkspaceFolders []string
	// LockDir is where the lock file is written. See LockDir.
	LockDir string
	// RuneSocket is recorded in the lock file. See Lock.RuneSocket.
	RuneSocket string
	// OnToolsConnected, if set, is called each time a client connects to
	// ToolsPath.
	OnToolsConnected func()
}

// Server is a running IDE server. Close stops it and removes its lock
// file.
type Server struct {
	cfg      Config
	token    string
	port     int
	lockPath string
	srv      *http.Server
	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once

	mu     sync.Mutex
	closed bool
	conns  sync.WaitGroup
}

// Start listens on a random loopback port, writes the lock file and
// serves until ctx is done or Close is called.
func Start(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.IDE == nil || cfg.Tools == nil {
		return nil, errors.New("ide and tools servers are required")
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	sctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg:    cfg,
		token:  token,
		port:   lis.Addr().(*net.TCPAddr).Port,
		ctx:    sctx,
		cancel: cancel,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(IDEPath+"{$}", func(w http.ResponseWriter, r *http.Request) {
		s.serve(w, r, cfg.IDE, nil)
	})
	mux.HandleFunc(ToolsPath, func(w http.ResponseWriter, r *http.Request) {
		s.serve(w, r, cfg.Tools, cfg.OnToolsConnected)
	})
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return sctx },
	}

	s.lockPath, err = WriteLock(cfg.LockDir, s.port, Lock{
		PID:              os.Getpid(),
		WorkspaceFolders: cfg.WorkspaceFolders,
		IDEName:          IDEName,
		Transport:        TransportWebSocket,
		AuthToken:        token,
		RuneSocket:       cfg.RuneSocket,
	})
	if err != nil {
		cancel()
		_ = lis.Close()
		return nil, err
	}

	go func() {
		if err := s.srv.Serve(lis); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			slog.Error("ide server stopped", "error", err)
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-sctx.Done():
		}
	}()
	return s, nil
}

// Port returns the port the server listens on.
func (s *Server) Port() int { return s.port }

// LockPath returns the path of the lock file.
func (s *Server) LockPath() string { return s.lockPath }

// Close disconnects every client, stops listening and removes the lock
// file. It is safe to call more than once.
func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		rmErr := os.Remove(s.lockPath)
		if errors.Is(rmErr, os.ErrNotExist) {
			rmErr = nil
		}
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.cancel()
		closeErr := s.srv.Close()
		s.conns.Wait()
		err = errors.Join(rmErr, closeErr)
	})
	return err
}

func (s *Server) serve(
	w http.ResponseWriter, r *http.Request,
	mcpSrv *server.MCPServer, onConnect func(),
) {
	got := r.Header.Get(AuthHeader)
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.track() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	defer s.conns.Done()
	// The token, which a browser cannot send on a WebSocket upgrade,
	// authenticates the client; Accept still refuses foreign origins.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{Subprotocol},
	})
	if err != nil {
		slog.Debug("websocket upgrade", "path", r.URL.Path, "error", err)
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(maxMessageSize)
	if onConnect != nil {
		onConnect()
	}
	if err := serveMCP(r.Context(), mcpSrv, conn); err != nil {
		slog.Debug("mcp connection closed", "path", r.URL.Path, "error", err)
	}
}

// track counts a connection Close waits for, unless the server is
// closed.
func (s *Server) track() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns.Add(1)
	return true
}

// newToken returns 16 random bytes as 32 lowercase hex characters.
func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

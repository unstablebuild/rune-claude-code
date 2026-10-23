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

// Command rune-claude connects Claude Code to the Rune workspace it
// runs in.
//
//	rune-claude mcp
//
// serves the workspace's Rune tools as a stdio MCP server, bridging to
// the Claude Code extension of the Rune workspace that contains the
// working directory. The Claude Code plugin for Rune runs it.
//
//	rune-claude claude [args...]
//
// runs Claude Code with args, connected as its IDE to that same
// workspace. Rune's claude command alias runs it.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/unstablebuild/rune-claude-code/internal/ideserver"
)

const usage = `usage: rune-claude <command>

Commands:
  mcp                Serve the Rune workspace's tools to Claude Code over stdio.
  claude [args...]   Run Claude Code connected to the Rune workspace.
`

// Claude Code reads these variables. Given ssePortEnv, it connects to
// the IDE that listens on that port, ignoring the others.
const (
	autoConnectIDEEnv = "CLAUDE_CODE_AUTO_CONNECT_IDE"
	ssePortEnv        = "CLAUDE_CODE_SSE_PORT"
)

// maxMessageSize bounds a single JSON-RPC message from Rune.
const maxMessageSize = 64 << 20

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch cmd, args := os.Args[1], os.Args[2:]; {
	case cmd == "mcp" && len(args) == 0:
		ctx, stop := signal.NotifyContext(context.Background(),
			os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := serveMCP(ctx, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "rune-claude:", err)
			stop()
			os.Exit(1)
		}
	case cmd == "claude":
		err := runClaude(args)
		fmt.Fprintln(os.Stderr, "rune-claude:", err)
		os.Exit(1)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// runClaude replaces this process with Claude Code, run with args and
// pinned to the IDE connection of the workspace findLock picks. On its
// own, Claude Code connects only when exactly one IDE's folder contains
// its working directory, which nested Rune workspaces break. It returns
// only when Claude Code cannot be started.
func runClaude(args []string) error {
	path, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("find Claude Code: %w", err)
	}
	port := 0
	if lock, err := findLock(); err != nil {
		fmt.Fprintf(os.Stderr, "rune-claude: %v; starting Claude Code anyway\n", err)
	} else {
		port = lock.Port
	}
	err = syscall.Exec(path, append([]string{"claude"}, args...),
		claudeEnv(os.Environ(), port))
	return fmt.Errorf("run %s: %w", path, err)
}

// claudeEnv returns env with Claude Code's IDE auto-connect turned on,
// even where the user turned it off, since running Claude Code through
// rune-claude asks to connect. A port other than zero pins the
// connection to the IDE that listens on it.
func claudeEnv(env []string, port int) []string {
	set := []string{autoConnectIDEEnv + "=true"}
	if port != 0 {
		set = append(set, ssePortEnv+"="+strconv.Itoa(port))
	}
	out := make([]string, 0, len(env)+len(set))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.ContainsFunc(set, func(s string) bool {
			return strings.HasPrefix(s, name+"=")
		}) {
			out = append(out, kv)
		}
	}
	return append(out, set...)
}

// serveMCP bridges in and out, a stdio MCP transport, to the tools of
// the Rune workspace that contains the working directory. Without one,
// it answers the first request with an error saying how to get one.
func serveMCP(ctx context.Context, in io.Reader, out io.Writer) error {
	lock, err := findLock()
	if err != nil {
		replyError(in, out, err)
		return err
	}
	conn, err := dial(ctx, lock)
	if err != nil {
		err = fmt.Errorf("connect to Rune on port %d: %w", lock.Port, err)
		replyError(in, out, err)
		return err
	}
	return bridge(ctx, conn, in, out)
}

func findLock() (ideserver.LockFile, error) {
	dir, err := ideserver.LockDir()
	if err != nil {
		return ideserver.LockFile{}, err
	}
	locks, err := ideserver.ReadLocks(dir)
	if err != nil {
		return ideserver.LockFile{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ideserver.LockFile{}, err
	}
	lock, ok := pick(locks, cwd, os.Getenv("RUNE_SOCKET"), alive)
	if !ok {
		return ideserver.LockFile{}, fmt.Errorf("no Rune workspace with "+
			"the Claude Code extension contains %s; run Claude Code in a "+
			"Rune terminal after installing it with: pkg install claude-code", cwd)
	}
	return lock, nil
}

// pick returns the lock of the Rune workspace whose folder contains
// dir. When workspace folders nest, it prefers the workspace named by
// socket, the RUNE_SOCKET of the terminal Claude Code runs in, then the
// innermost folder.
func pick(
	locks []ideserver.LockFile, dir, socket string, alive func(int) bool,
) (ideserver.LockFile, bool) {
	dirs := withRealPath(dir)
	var best ideserver.LockFile
	bestRank := -1
	for _, l := range locks {
		if l.IDEName != ideserver.IDEName ||
			l.Transport != ideserver.TransportWebSocket || !alive(l.PID) {
			continue
		}
		depth := -1
		for _, f := range l.WorkspaceFolders {
			for _, folder := range withRealPath(f) {
				for _, d := range dirs {
					if contains(folder, d) {
						depth = max(depth, len(folder))
					}
				}
			}
		}
		if depth < 0 {
			continue
		}
		// Matching the socket outranks any depth.
		rank := depth
		if socket != "" && l.RuneSocket == socket {
			rank += 1 << 30
		}
		if rank > bestRank {
			best, bestRank = l, rank
		}
	}
	return best, bestRank >= 0
}

// withRealPath returns path and, if it differs, path with symbolic
// links resolved.
func withRealPath(path string) []string {
	path = filepath.Clean(path)
	real, err := filepath.EvalSymlinks(path)
	if err != nil || real == path {
		return []string{path}
	}
	return []string{path, real}
}

// contains reports whether dir is folder or inside it, as Claude Code
// matches its working directory against workspace folders.
func contains(folder, dir string) bool {
	return dir == folder ||
		strings.HasPrefix(dir, strings.TrimSuffix(folder, string(filepath.Separator))+
			string(filepath.Separator))
}

// alive reports whether a process with pid runs.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func dial(ctx context.Context, lock ideserver.LockFile) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := "ws://127.0.0.1:" + strconv.Itoa(lock.Port) + ideserver.ToolsPath
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader:   map[string][]string{ideserver.AuthHeader: {lock.AuthToken}},
		Subprotocols: []string{ideserver.Subprotocol},
	})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(maxMessageSize)
	return conn, nil
}

// bridge copies newline-delimited messages from in to conn and messages
// from conn to out, until in ends, conn closes or ctx is done.
func bridge(ctx context.Context, conn *websocket.Conn, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() { _ = conn.CloseNow() }()

	errc := make(chan error, 2)
	go func() {
		r := bufio.NewReader(in)
		for {
			line, err := r.ReadBytes('\n')
			if msg := strings.TrimSpace(string(line)); msg != "" {
				if werr := conn.Write(ctx, websocket.MessageText, []byte(msg)); werr != nil {
					errc <- fmt.Errorf("send to Rune: %w", werr)
					return
				}
			}
			if errors.Is(err, io.EOF) {
				errc <- nil
				return
			}
			if err != nil {
				errc <- err
				return
			}
		}
	}()
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				if ctx.Err() != nil {
					errc <- nil
				} else {
					errc <- fmt.Errorf("connection to Rune closed: %w", err)
				}
				return
			}
			if _, err := out.Write(append(data, '\n')); err != nil {
				errc <- err
				return
			}
		}
	}()
	select {
	case err := <-errc:
		if err == nil {
			_ = conn.Close(websocket.StatusNormalClosure, "")
		}
		return err
	case <-ctx.Done():
		return nil
	}
}

// replyIdle bounds how long replyError waits for the next request.
const replyIdle = 5 * time.Second

// replyError answers the requests read from in with err until it has
// answered initialize, the error Claude Code shows for the server. A
// client may first probe with other requests, such as server/discover,
// and fall back to initialize when they fail. It stops early when in
// closes or no request arrives for replyIdle.
func replyError(in io.Reader, out io.Writer, err error) {
	lines := make(chan []byte)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer close(lines)
		r := bufio.NewReader(in)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				select {
				case lines <- line:
				case <-done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		var line []byte
		select {
		case l, ok := <-lines:
			if !ok {
				return
			}
			line = l
		case <-time.After(replyIdle):
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(line, &req) != nil || len(req.ID) == 0 {
			continue
		}
		resp, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error":   map[string]any{"code": -32000, "message": err.Error()},
		})
		_, _ = out.Write(append(resp, '\n'))
		if req.Method == "initialize" {
			return
		}
	}
}

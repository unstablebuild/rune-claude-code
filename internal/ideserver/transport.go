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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// notificationBuffer bounds the notifications queued for a client that
// is not reading; further ones are dropped.
const notificationBuffer = 64

// session is the mcp-go client session of one WebSocket connection.
type session struct {
	id            string
	notifications chan mcp.JSONRPCNotification
	initialized   atomic.Bool
}

var _ server.ClientSession = (*session)(nil)

func newSession() *session {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return &session{
		id:            hex.EncodeToString(b[:]),
		notifications: make(chan mcp.JSONRPCNotification, notificationBuffer),
	}
}

func (s *session) SessionID() string { return s.id }
func (s *session) Initialize()       { s.initialized.Store(true) }
func (s *session) Initialized() bool { return s.initialized.Load() }
func (s *session) NotificationChannel() chan<- mcp.JSONRPCNotification {
	return s.notifications
}

// serveMCP serves s over conn, one JSON-RPC message per text frame,
// until conn or ctx closes. Tool calls run concurrently, since some
// (openDiff) block until the user acts; every other message is handled
// in order.
func serveMCP(
	ctx context.Context, s *server.MCPServer, conn *websocket.Conn,
) error {
	ctx, cancel := context.WithCancel(ctx)
	sess := newSession()
	if err := s.RegisterSession(ctx, sess); err != nil {
		cancel()
		return err
	}
	ctx = s.WithContext(ctx, sess)

	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
		s.UnregisterSession(context.Background(), sess.SessionID())
	}()

	write := func(msg any) {
		data, err := json.Marshal(msg)
		if err != nil {
			slog.Error("marshal mcp message", "error", err)
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			slog.Debug("write mcp message", "error", err)
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case n := <-sess.notifications:
				write(n)
			case <-ctx.Done():
				return
			}
		}
	}()

	handle := func(data []byte) {
		if resp := s.HandleMessage(ctx, data); resp != nil {
			write(resp)
		}
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure ||
				websocket.CloseStatus(err) == websocket.StatusGoingAway ||
				errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		if !isToolCall(data) {
			handle(data)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle(data)
		}()
	}
}

func isToolCall(data []byte) bool {
	var msg struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(data, &msg) == nil &&
		msg.Method == string(mcp.MethodToolsCall)
}

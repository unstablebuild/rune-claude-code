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

// Command extension_claude_code is the Rune extension that connects
// Claude Code to a Rune workspace: Claude Code finds it as an IDE, and
// the rune-claude helper bridges its tools to Claude Code.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/api/extensionapi"

	"github.com/unstablebuild/rune-claude-code/internal/difftab"
	"github.com/unstablebuild/rune-claude-code/internal/ideserver"
	"github.com/unstablebuild/rune-claude-code/internal/idetools"
	"github.com/unstablebuild/rune-claude-code/internal/reminder"
	"github.com/unstablebuild/rune-claude-code/internal/tools"
	"github.com/unstablebuild/rune-claude-code/internal/version"
)

func main() {
	meta := extensionapi.Metadata{
		DeveloperID:      "Unstable Build",
		DeveloperEmail:   "it@unstable.build",
		DeveloperKey:     "064D4ABCFA6D9338",
		ExtensionID:      "claude-code",
		ExtensionName:    "Claude Code",
		ExtensionVersion: version.Version,
		Permissions: extensionapi.NewPermissions(
			extensionapi.PermissionEditor,
			extensionapi.PermissionCommands,
			extensionapi.PermissionFileSystem,
			extensionapi.PermissionBrowserWindowManager,
			extensionapi.PermissionBrowserResourceOpener,
			extensionapi.PermissionNotifications,
			extensionapi.PermissionInterrupt,
			extensionapi.PermissionStorage,
			extensionapi.PermissionLSP,
			extensionapi.PermissionSyntaxTree,
			extensionapi.PermissionConfig,
		),
	}
	ext := &extension{}
	err := extensionapi.ServeWorkspaceExtension(ext, meta)
	// The extension's context is only canceled once serving returns, so
	// remove the lock file here rather than racing the process exit.
	ext.close()
	if err != nil {
		slog.Error("claude code extension exited", "error", err)
		os.Exit(1)
	}
}

type extension struct {
	mu      sync.Mutex
	closers []func()
}

func (e *extension) ExtendWorkspace(
	ctx context.Context, ws *extensionapi.Workspace, _ config.Config,
) error {
	fs := ws.FileSystem(ctx)
	root, err := fs.URI(".")
	if err != nil {
		return fmt.Errorf("workspace root: %w", err)
	}
	editor := ws.Editor(ctx)
	wm := ws.WindowManager(ctx)
	ideDeps := idetools.Deps{
		Editor:        editor,
		LSP:           ws.LSP(ctx),
		Opener:        ws.ResourceOpener(ctx),
		WindowManager: wm,
		FileSystem:    fs,
		Root:          root,
	}

	ide := idetools.New(ideDeps)
	diffs := difftab.New(difftab.Deps{
		WindowManager: wm,
		Opener:        ideDeps.Opener,
		FileSystem:    fs,
		Interrupter:   ws.Interrupter(ctx),
		Root:          root.Path(),
	})
	diffs.Register(ide.Server())
	e.onClose(diffs.Close)
	t := tools.New(ctx, tools.Deps{
		IDE:           ideDeps,
		Parser:        ws.Parser(ctx),
		Notifications: ws.Notifications(ctx),
	})

	if err := editor.SubscribeEvents(idetools.Events, ide); err != nil {
		return fmt.Errorf("subscribe ide events: %w", err)
	}
	if err := editor.SubscribeEvents(difftab.Events, diffs); err != nil {
		return fmt.Errorf("subscribe diff events: %w", err)
	}
	if err := editor.SubscribeEvents(tools.Events, t); err != nil {
		return fmt.Errorf("subscribe tool events: %w", err)
	}
	if err := ws.RegisterCommand(idetools.Command, ide); err != nil {
		return fmt.Errorf("register command: %w", err)
	}

	remind := reminder.New(ws.Storage(ctx), wm)
	lockDir, err := ideserver.LockDir()
	if err != nil {
		return err
	}
	srv, err := ideserver.Start(ctx, ideserver.Config{
		IDE:              ide.Server(),
		Tools:            t.Server(),
		WorkspaceFolders: workspaceFolders(root.Path()),
		LockDir:          lockDir,
		RuneSocket:       os.Getenv("RUNE_SOCKET"),
		OnToolsConnected: func() {
			go func() {
				if err := remind.Dismiss(ctx); err != nil {
					slog.Warn("dismiss plugin reminder", "error", err)
				}
			}()
		},
	})
	if err != nil {
		return fmt.Errorf("start ide server: %w", err)
	}
	e.onClose(func() {
		if err := srv.Close(); err != nil {
			slog.Warn("close ide server", "error", err)
		}
	})
	slog.Info("claude code extension ready",
		"workspace", root.Path(), "lock", srv.LockPath())

	if err := remind.Show(ctx); err != nil {
		slog.Warn("plugin reminder", "error", err)
	}
	return nil
}

// workspaceFolders returns root and, if it differs, root with symbolic
// links resolved, since Claude Code compares them to its working
// directory as written.
func workspaceFolders(root string) []string {
	real, err := filepath.EvalSymlinks(root)
	if err != nil || real == root {
		return []string{root}
	}
	return []string{root, real}
}

func (e *extension) onClose(fn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closers = append(e.closers, fn)
}

// close stops the IDE server, which removes its lock file, then the
// rest, in reverse order.
func (e *extension) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.closers) - 1; i >= 0; i-- {
		e.closers[i]()
	}
	e.closers = nil
}

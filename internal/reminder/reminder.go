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

// Package reminder asks the user to install the Claude Code plugin
// that completes the integration, until they say they did or the
// plugin connects.
package reminder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/storageapi"
	"github.com/unstablebuild/rune-go-sdk/component"
	"github.com/unstablebuild/rune-go-sdk/handler"
)

// Key is the storage document that records the reminder was dismissed.
// Storage is shared by every workspace of a Rune process, so it is
// dismissed everywhere at once.
const Key = "plugin_reminder_dismissed"

// Message is the reminder's text.
const Message = "Finish connecting Claude Code to Rune. In Claude Code, run:\n\n" +
	"  /plugin marketplace add unstablebuild/rune-claude-code\n" +
	"  /plugin install rune@unstablebuild\n\n" +
	"Claude Code sessions you start in new Rune terminals then connect to Rune."

// Option is the reminder's only button.
const Option = "Already installed"

// dismissal is the document stored under Key.
type dismissal struct {
	Dismissed bool
}

// Reminder shows the reminder in a floating window.
type Reminder struct {
	storage storageapi.Service
	wm      browserapi.WindowManager

	mu sync.Mutex
	// win is the reminder's window while it is open.
	win       browserapi.Window
	showing   bool
	dismissed bool
}

// New returns a Reminder.
func New(storage storageapi.Service, wm browserapi.WindowManager) *Reminder {
	return &Reminder{storage: storage, wm: wm}
}

// Show opens the reminder unless it was dismissed. Closing it without
// pressing its button shows it again next time.
func (r *Reminder) Show(ctx context.Context) error {
	var d dismissal
	err := r.storage.Get(ctx, Key, &d)
	if err != nil && !errors.Is(err, storageapi.ErrNotFound) {
		return fmt.Errorf("read %s: %w", Key, err)
	}
	r.mu.Lock()
	skip := d.Dismissed || r.dismissed || r.showing
	r.showing = true
	r.mu.Unlock()
	if skip {
		return nil
	}
	prompt := handler.NewPrompt(handler.PromptConfig{
		PromptConfig: component.PromptConfig{
			Message: Message,
			Options: []string{Option},
		},
		PromptHandler: handler.FuncPromptHandler(
			func(int, string) {
				// The host waits for this call; storage is reached
				// through it.
				go func() {
					if err := r.dismiss(ctx, false); err != nil {
						slog.Warn("dismiss plugin reminder", "error", err)
					}
				}()
			},
			func() error {
				r.mu.Lock()
				r.win, r.showing = nil, false
				r.mu.Unlock()
				return nil
			},
		),
	})
	win, err := r.wm.Floating(prompt, browserapi.FloatingConfig{
		Alignment: component.AlignmentCentered,
		Title:     "Claude Code",
	})
	r.mu.Lock()
	if err != nil {
		r.showing = false
		r.mu.Unlock()
		return fmt.Errorf("show plugin reminder: %w", err)
	}
	dismissed := r.dismissed
	if !dismissed {
		r.win = win
	}
	r.mu.Unlock()
	if dismissed {
		// Dismissed while opening.
		if err := r.wm.CloseWindow(win); err != nil {
			slog.Debug("close plugin reminder", "error", err)
		}
	}
	return nil
}

// Dismiss records that the plugin is installed and closes the reminder
// if it is open. Only the first call does anything.
func (r *Reminder) Dismiss(ctx context.Context) error {
	return r.dismiss(ctx, true)
}

func (r *Reminder) dismiss(ctx context.Context, closeWindow bool) error {
	r.mu.Lock()
	if r.dismissed {
		r.mu.Unlock()
		return nil
	}
	r.dismissed = true
	win := r.win
	r.win = nil
	r.mu.Unlock()
	if closeWindow && win != nil {
		if err := r.wm.CloseWindow(win); err != nil {
			slog.Debug("close plugin reminder", "error", err)
		}
	}
	if err := r.storage.Set(ctx, Key, dismissal{Dismissed: true}); err != nil {
		return fmt.Errorf("write %s: %w", Key, err)
	}
	return nil
}

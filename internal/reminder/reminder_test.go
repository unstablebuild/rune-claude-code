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

package reminder

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/browserapi"
	"github.com/unstablebuild/rune-go-sdk/api/storageapi"
	"github.com/unstablebuild/rune-go-sdk/api/storageapi/storagestub"
	"github.com/unstablebuild/rune-go-sdk/component"
	"github.com/unstablebuild/rune-go-sdk/handler"
	"github.com/unstablebuild/rune-go-sdk/term"
)

func TestButtonDismisses(t *testing.T) {
	store := storagestub.NewInMemoryService()
	wm := &fakeWM{}
	r := New(store, wm)
	require.NoError(t, r.Show(context.Background()))
	require.Len(t, wm.floating, 1)
	assert.Equal(t, browserapi.FloatingConfig{
		Alignment: component.AlignmentCentered, Title: "Claude Code",
	}, wm.configs[0])
	assert.Contains(t, Message, "/plugin marketplace add unstablebuild/rune-claude-code")
	assert.Contains(t, Message, "/plugin install rune@unstablebuild")

	// Showing again while open does nothing.
	require.NoError(t, r.Show(context.Background()))
	assert.Len(t, wm.floating, 1)

	p := wm.floating[0].(*handler.Prompt)
	exit, _ := p.Handle(term.Event{Type: term.EventKey, Key: term.KeyEnter})
	assert.True(t, exit)
	require.Eventually(t, func() bool { return dismissed(t, store) },
		5*time.Second, time.Millisecond)
	assert.Empty(t, wm.closedWindows(), "the prompt closes itself")

	// Another workspace, or the next start, does not show it.
	wm2 := &fakeWM{}
	require.NoError(t, New(store, wm2).Show(context.Background()))
	assert.Empty(t, wm2.floating)
}

func TestCloseKeepsReminding(t *testing.T) {
	store := storagestub.NewInMemoryService()
	wm := &fakeWM{}
	r := New(store, wm)
	require.NoError(t, r.Show(context.Background()))
	require.NoError(t, wm.floating[0].Close())
	assert.False(t, dismissed(t, store))

	require.NoError(t, r.Show(context.Background()))
	assert.Len(t, wm.floating, 2)
}

func TestButtonLooksLikeHostPrompts(t *testing.T) {
	wm := &fakeWM{}
	require.NoError(t, New(storagestub.NewInMemoryService(), wm).Show(context.Background()))
	p := wm.floating[0].(*handler.Prompt)
	width, height := p.Dimensions()
	p.Resize(width, height)
	w := term.NewStringWriter(width, height)
	p.Draw(w)
	require.NoError(t, w.Flush())

	rows := strings.Split(w.String(), "\n")
	cells := w.Cells()
	for y, row := range rows {
		x := strings.Index(row, Option)
		if x < 0 {
			continue
		}
		button := cells[y*width : (y+1)*width]
		for i := x - 2; i < x+len(Option)+2; i++ {
			assert.Equal(t, ButtonAttr.Bg, button[i].Bg, "cell %d", i)
			assert.Equal(t, ButtonAttr.Fg, button[i].Fg, "cell %d", i)
		}
		assert.NotEqual(t, ButtonAttr.Bg, button[x-3].Bg, "two cells of padding on the left")
		assert.NotEqual(t, ButtonAttr.Bg, button[x+len(Option)+2].Bg, "two cells of padding on the right")
		return
	}
	t.Fatalf("no button in:\n%s", w.String())
}

func TestPluginConnectionDismisses(t *testing.T) {
	store := storagestub.NewInMemoryService()
	wm := &fakeWM{}
	r := New(store, wm)
	require.NoError(t, r.Show(context.Background()))

	require.NoError(t, r.Dismiss(context.Background()))
	assert.True(t, dismissed(t, store))
	assert.Equal(t, []uint64{1}, wm.closedWindows())

	require.NoError(t, r.Dismiss(context.Background()))
	assert.Equal(t, []uint64{1}, wm.closedWindows())
	require.NoError(t, r.Show(context.Background()))
	assert.Len(t, wm.floating, 1)
}

func TestDismissWithoutReminder(t *testing.T) {
	store := storagestub.NewInMemoryService()
	wm := &fakeWM{}
	r := New(store, wm)
	require.NoError(t, r.Dismiss(context.Background()))
	assert.True(t, dismissed(t, store))
	assert.Empty(t, wm.closedWindows())
	require.NoError(t, r.Show(context.Background()))
	assert.Empty(t, wm.floating)
}

func dismissed(t *testing.T, store storageapi.Service) bool {
	t.Helper()
	var d dismissal
	err := store.Get(context.Background(), Key, &d)
	if err != nil {
		require.ErrorIs(t, err, storageapi.ErrNotFound)
	}
	return d.Dismissed
}

type fakeWindow struct{ id uint64 }

func (w fakeWindow) WindowID() uint64 { return w.id }

type fakeWM struct {
	browserapi.WindowManager
	floating []browserapi.Floating
	configs  []browserapi.FloatingConfig

	mu     sync.Mutex
	closed []uint64
}

func (w *fakeWM) Floating(
	h browserapi.Floating, cfg browserapi.FloatingConfig,
) (browserapi.Window, error) {
	w.floating = append(w.floating, h)
	w.configs = append(w.configs, cfg)
	return fakeWindow{uint64(len(w.floating))}, nil
}

func (w *fakeWM) CloseWindow(win browserapi.Window) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = append(w.closed, win.WindowID())
	return nil
}

func (w *fakeWM) closedWindows() []uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

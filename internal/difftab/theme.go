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
	"errors"
	"log/slog"
	"strings"

	"github.com/unstablebuild/rune-go-sdk/api/config"
	"github.com/unstablebuild/rune-go-sdk/term"
)

// Theme holds the attributes a review draws with. ThemeFrom reads them
// from Rune's configuration, so a review looks like Rune's editor.
type Theme struct {
	// Text is the editor's text, editor.<mode>.attr.
	Text term.Attributes
	// LineNumber is the gutter's, editor.aux_bar.line_number_attr.
	LineNumber term.Attributes
	// Insert and Delete tint inserted and deleted lines, in the diff
	// and in the proposal Edit opens; InsertWord and DeleteWord the words
	// that changed in them. Rune has no keys for these, so they are
	// palette colors, which the theme sets.
	Insert, Delete         term.Attributes
	InsertWord, DeleteWord term.Attributes
	// InsertMark and DeleteMark mark the changes in the proposal Edit
	// opens; their text is white, which reads on any theme's green and
	// red where the editor's text may not.
	InsertMark, DeleteMark term.Attributes
	// Inserted and Deleted count the changed lines in the bottom bar,
	// editor.aux_bar.git_add_inline_attr and git_del_inline_attr.
	Inserted, Deleted term.Attributes
	// Button and Selected are the buttons' and the selected one's, as
	// the options of Rune's prompts, browser.prompt.text_attr and
	// highlight_attr.
	Button, Selected term.Attributes
	// TabSpaces is the cells a tab takes, editor.tabspaces.
	TabSpaces int
}

// DefaultTheme is Rune's own defaults for the keys ThemeFrom reads.
var DefaultTheme = Theme{
	LineNumber: term.Attributes{Fg: term.ColorGray},
	Insert:     term.Attributes{Bg: term.ColorGreen},
	Delete:     term.Attributes{Bg: term.ColorRed},
	InsertWord: term.Attributes{Bg: term.ColorLime},
	DeleteWord: term.Attributes{Bg: term.ColorMaroon},
	InsertMark: term.Attributes{Fg: term.ColorWhite, Bg: term.ColorGreen},
	DeleteMark: term.Attributes{Fg: term.ColorWhite, Bg: term.ColorRed},
	Inserted:   term.Attributes{Fg: term.ColorGreen, Attrs: term.AttrBold},
	Deleted:    term.Attributes{Fg: term.ColorMaroon, Attrs: term.AttrBold},
	Button:     term.Attributes{Bg: term.ColorGray, Attrs: term.AttrBold},
	Selected:   term.Attributes{Bg: term.ColorRed, Attrs: term.AttrBold},
	TabSpaces:  4,
}

// ThemeFrom reads a Theme from Rune's configuration, keeping
// DefaultTheme's attributes for the keys it does not set.
func ThemeFrom(cfg config.Config) Theme {
	t := DefaultTheme
	editor := section(cfg, "editor")
	mode := "vim"
	if m, err := get(editor, config.Config.GetString, "mode"); err == nil {
		mode = editorMode(m)
	}
	if n, err := get(editor, config.Config.GetInt, "tabspaces"); err == nil && n > 0 {
		t.TabSpaces = n
	}
	attrs(&t.Text, section(editor, mode), "attr")
	bar := section(editor, "aux_bar")
	attrs(&t.LineNumber, bar, "line_number_attr")
	attrs(&t.Inserted, bar, "git_add_inline_attr")
	attrs(&t.Deleted, bar, "git_del_inline_attr")
	prompt := section(cfg, "browser", "prompt")
	attrs(&t.Button, prompt, "text_attr")
	attrs(&t.Selected, prompt, "highlight_attr")
	return t
}

// editorMode is the editor.<mode> section Rune reads for mode.
func editorMode(mode string) string {
	switch strings.ToLower(mode) {
	case "modal":
		return "vim"
	case "modeless":
		return "standard"
	}
	return strings.ToLower(mode)
}

// section returns the configuration at path under cfg, or nil.
func section(cfg config.Config, path ...string) config.Config {
	for _, key := range path {
		if cfg == nil {
			return nil
		}
		sub, err := cfg.GetConfig(key)
		if err != nil {
			logLookup(key, err)
			return nil
		}
		cfg = sub
	}
	return cfg
}

func get[T any](cfg config.Config, fn func(config.Config, string) (T, error), key string) (T, error) {
	if cfg == nil {
		var zero T
		return zero, config.ErrNotFound
	}
	v, err := fn(cfg, key)
	logLookup(key, err)
	return v, err
}

// attrs sets dst to the attributes at key in cfg, if there are any.
func attrs(dst *term.Attributes, cfg config.Config, key string) {
	if a, err := get(cfg, config.GetAttributes, key); err == nil {
		*dst = a
	}
}

func logLookup(key string, err error) {
	if err != nil && !errors.Is(err, config.ErrNotFound) {
		slog.Debug("read diff theme", "key", key, "error", err)
	}
}

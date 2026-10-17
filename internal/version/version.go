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

// Package version identifies this build of the Rune extension for
// Claude Code.
package version

// Version is the release of the extension, the rune-claude helper and
// the Claude Code plugin, which are tagged together.
const Version = "0.1.2"

// Protocol is the version of the contract between the plugin's mod and
// the extension's tools. It changes when a tool the mod calls changes
// incompatibly, so the mod can ask the user to update.
const Protocol = 2

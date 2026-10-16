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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const lockSuffix = ".lock"

// LockDir returns the directory Claude Code reads lock files from:
// $CLAUDE_CONFIG_DIR/ide, or ~/.claude/ide.
func LockDir() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "ide"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "ide"), nil
}

// LockPath returns the path of the lock file for port in dir.
func LockPath(dir string, port int) string {
	return filepath.Join(dir, strconv.Itoa(port)+lockSuffix)
}

// WriteLock writes lock as the lock file for port in dir, readable
// only by the user, and returns its path. The file is written to a
// temporary name and renamed so a reader never sees a partial lock.
func WriteLock(dir string, port int, lock Lock) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	data, err := json.Marshal(lock)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".rune-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create lock file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("chmod lock file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write lock file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close lock file: %w", err)
	}
	path := LockPath(dir, port)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("rename lock file: %w", err)
	}
	return path, nil
}

// LockFile is a lock file read from disk.
type LockFile struct {
	Port int
	Lock
}

// ReadLocks reads every well-formed lock file in dir. A missing dir
// has no lock files.
func ReadLocks(dir string) ([]LockFile, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var locks []LockFile
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), lockSuffix)
		if !ok || e.IsDir() {
			continue
		}
		port, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var lock Lock
		if json.Unmarshal(data, &lock) != nil {
			continue
		}
		locks = append(locks, LockFile{Port: port, Lock: lock})
	}
	return locks, nil
}

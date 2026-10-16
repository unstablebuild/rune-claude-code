GO ?= go
GOTESTFLAGS ?= -race -timeout 120s

BIN=bin
PKG=pkg
TAR=claude-code.tar.gz
EXECSRC=$(wildcard cmd/**/*.go)
LIBSRC=$(wildcard internal/**/*.go)
EXECDIRS=$(sort $(dir $(EXECSRC)))
EXECS=$(patsubst cmd/%/,$(BIN)/%,$(EXECDIRS))
PKGCMDS=extension_claude_code rune-claude
# What the release archive holds, sorted.
PKGENTRIES=./bin/extension_claude_code ./bin/rune-claude ./config.yaml

HOST_OS := $(shell uname | tr '[:upper:]' '[:lower:]')
HOST_ARCH := $(shell uname -m | sed -e 's/^x86_64$$/amd64/' -e 's/^aarch64$$/arm64/')
TARGET_OS ?= $(HOST_OS)
TARGET_ARCH ?= $(HOST_ARCH)

# Release targets select their bluectl project and bucket from the local,
# gitignored deploy/bluectl matrix; provision it before publishing.
BLUECTL_CONFIG_ROOT := $(abspath deploy/bluectl)

DIST_TARGETS := \
	dist-prod-darwin-arm64 dist-prod-darwin-amd64 \
	dist-prod-linux-arm64  dist-prod-linux-amd64  \
	dist-staging-darwin-arm64 dist-staging-darwin-amd64 \
	dist-staging-linux-arm64  dist-staging-linux-amd64

.PHONY: default clean test lint format license assert_license \
	plugin-validate plugin-test pkg $(TAR) verify verify-tag \
	$(DIST_TARGETS) dist-prod-all dist-staging-all

default: $(EXECS)

test:
	@ $(GO) test ./... $(GOTESTFLAGS)

lint:
	@ golangci-lint run

format:
	@ $(GO) fmt ./...

license:
	@ bluectl license LICENSE `find . -name \*.go | xargs`

assert_license:
	@ bluectl license -d LICENSE `find . -name \*.go | xargs`

plugin-validate:
	@ claude plugin validate plugin

plugin-test:
	@ claude plugin test plugin

clean:
	@ rm -rf $(BIN) $(PKG) $(TAR)

$(BIN):
	@ mkdir -p $(BIN)

$(BIN)/%: $(EXECSRC) $(LIBSRC) | $(BIN)
	@ CGO_ENABLED=0 $(GO) build -o $@ ./$(patsubst $(BIN)/%,cmd/%,$@)

# Stage the package for TARGET_OS/TARGET_ARCH. Releases build with cgo off,
# so any host can package any target. macOS binaries ship without a
# Developer ID signature or notarization: Rune installs them itself,
# verifying the tarball's PGP signature and clearing quarantine, and the Go
# linker ad-hoc signs darwin/arm64 binaries, which Apple Silicon requires.
pkg:
	@ rm -rf $(PKG) && mkdir -p $(PKG)/bin
	@ CGO_ENABLED=0 GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) \
		$(GO) build -o $(PKG)/bin/ $(addprefix ./cmd/,$(PKGCMDS))
	@ cp config.yaml $(PKG)/

# The archive is rooted at pkg/'s contents, so config.yaml sits at its root
# where the installer looks for it. COPYFILE_DISABLE and --no-xattrs keep a
# macOS host from adding AppleDouble ._ entries and extended attributes.
$(TAR): pkg
	@ cd $(PKG) && COPYFILE_DISABLE=1 tar --format=ustar --no-xattrs --no-acls \
		-czf ../$(TAR) .

# The archive must hold exactly the package's files, so no Go source, and
# its binaries must be cgo-off builds for TARGET_OS/TARGET_ARCH.
verify: $(TAR)
	@ entries=$$(tar -tzf $(TAR) | grep -v '/$$' | LC_ALL=C sort | xargs); \
	  if [ "$$entries" != "$(PKGENTRIES)" ]; then \
		echo "error: $(TAR) holds '$$entries', want '$(PKGENTRIES)'" >&2; \
		exit 1; \
	  fi
	@ for cmd in $(PKGCMDS); do \
		info=$$($(GO) version -m $(PKG)/bin/$$cmd) || exit 1; \
		for want in GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) CGO_ENABLED=0; do \
			printf '%s\n' "$$info" | grep -qxE "[[:space:]]*build[[:space:]]+$$want" || { \
				echo "error: $(PKG)/bin/$$cmd is not built with $$want" >&2; \
				exit 1; \
			}; \
		done; \
	  done
	@ echo "$(TAR) verified for $(TARGET_OS)/$(TARGET_ARCH)"

# The version dist.sh uploads as. Published versions are immutable and the
# plugin's update check compares versions, so only the exact, clean tag of
# version.Version, which plugin.json must match, is released.
RELEASE_TAG = $(shell git describe --tags --dirty 2>/dev/null)
VERSION = $(shell sed -n 's/^const Version = "\(.*\)"$$/\1/p' internal/version/version.go)
PLUGIN_VERSION = $(shell sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' plugin/.claude-plugin/plugin.json)

verify-tag:
	@ if [ '$(RELEASE_TAG)' != 'v$(VERSION)' ] || [ '$(PLUGIN_VERSION)' != '$(VERSION)' ]; then \
		echo "error: release tag '$(RELEASE_TAG)' must be the exact, clean tag v$(VERSION) of version.Version, and plugin.json's version '$(PLUGIN_VERSION)' must match it" >&2; \
		exit 1; \
	  fi

# Targets share claude-code.tar.gz and pkg/, so they must run one at a time.
dist-prod-all dist-staging-all: dist-%-all:
	$(MAKE) dist-$*-darwin-arm64
	$(MAKE) dist-$*-darwin-amd64
	$(MAKE) dist-$*-linux-arm64
	$(MAKE) dist-$*-linux-amd64

# Under Rune, each release reports to its terminal tab: working while it
# builds, tests and uploads, blocked while bluectl waits for the PGP
# passphrase (see dist.sh), then done or error, each with a notification.
# Older runectl builds lack `status wrap`, so those run the release unwrapped.
STATUS_WRAP = $(if $(shell runectl status wrap --help >/dev/null 2>&1 && echo y),runectl status wrap --app make --msg "$@" --)

# make runs a recipe line that names $(MAKE) even under -n, and this one
# ends in ./dist.sh, so it calls make through SUBMAKE to keep
# `make -n dist-*` from uploading.
SUBMAKE = $(MAKE)

# A target whose version is already in its bucket is skipped before
# building, so dist-*-all resumes after a partial run.
$(DIST_TARGETS): dist-%: verify-tag
	@ env=$$(echo $* | cut -d- -f1); \
	  os=$$(echo $* | cut -d- -f2); \
	  arch=$$(echo $* | cut -d- -f3); \
	  cfg=$(BLUECTL_CONFIG_ROOT)/$$env/$$os-$$arch; \
	  published=$$(bluectl -c $$cfg release list -F '{{.Version}}' claude-code) || exit 1; \
	  if printf '%s\n' "$$published" | grep -qxF '$(RELEASE_TAG)'; then \
		echo "claude-code@$(RELEASE_TAG) is already published for $$env $$os/$$arch; skipping"; \
		exit 0; \
	  fi; \
	  $(SUBMAKE) clean || exit 1; \
	  $(STATUS_WRAP) env BLUECTL_CONFIG_DIR=$$cfg \
	  BLUE_TARGET_OS=$$os BLUE_TARGET_ARCH=$$arch BLUE_RELEASE_TAR=$(TAR) \
	  sh -c '$(SUBMAKE) test && $(SUBMAKE) verify TARGET_OS=$$BLUE_TARGET_OS TARGET_ARCH=$$BLUE_TARGET_ARCH && ./dist.sh'
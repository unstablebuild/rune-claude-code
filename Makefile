GO ?= go
UNAME := $(shell uname)
CODESIGN_IDENTITY ?= Developer ID Application: Unstable Build, LLC. (YYZRWD888J)
GOTESTFLAGS ?= -race -timeout 120s

BIN=bin
PKG=pkg
TAR=claude-code.tar.gz
EXECSRC=$(wildcard cmd/**/*.go)
LIBSRC=$(wildcard internal/**/*.go)
EXECDIRS=$(sort $(dir $(EXECSRC)))
EXECS=$(patsubst cmd/%/,$(BIN)/%,$(EXECDIRS))
PKGBINS=$(BIN)/extension_claude_code $(BIN)/rune-claude

# Release targets select their Bluectl project from the local, gitignored
# deploy/bluectl matrix; provision it before publishing.
BLUECTL_HOST_OS := $(shell uname | awk '{print tolower($$0)}')
BLUECTL_HOST_ARCH := $(shell uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')
BLUECTL_CONFIG_ROOT := $(abspath deploy/bluectl)

.PHONY: default clean test lint format license assert_license \
	plugin-validate plugin-test pkg sign dist prod-dist staging-dist

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

$(PKG): $(PKGBINS)
	@ mkdir -p $(PKG)/bin
	@ cp $(PKGBINS) $(PKG)/bin/
	@ cp config.yaml $(PKG)/

ifeq ($(UNAME),Darwin)
sign: $(PKG)
	codesign --force --options runtime --sign "$(CODESIGN_IDENTITY)" $(PKG)/bin/*
else
sign: $(PKG)
	@ echo "Skipping codesign (not on macOS)"
endif

$(TAR): sign
	cd $(PKG) && tar -czvf ../$(TAR) .

dist: clean $(TAR)
	@: $${BLUECTL_CONFIG_DIR:?BLUECTL_CONFIG_DIR not set; use prod-dist or staging-dist}
	@ BLUE_RELEASE_TAR=$(TAR) ./dist.sh

prod-dist:
	@ BLUECTL_CONFIG_DIR=$(BLUECTL_CONFIG_ROOT)/prod/$(BLUECTL_HOST_OS)-$(BLUECTL_HOST_ARCH) $(MAKE) dist

staging-dist:
	@ BLUECTL_CONFIG_DIR=$(BLUECTL_CONFIG_ROOT)/staging/$(BLUECTL_HOST_OS)-$(BLUECTL_HOST_ARCH) $(MAKE) dist
VERSION ?= dev

# Install location: $(DESTDIR)$(PREFIX)/bin/skald. The default needs no sudo;
# use PREFIX=/usr/local (with sudo if required) for a system-wide install.
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
INSTALL_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test lint bench conc release release-check install update uninstall

build:
	go build -ldflags "-X skald/internal/cli.Version=$(VERSION)" -o skald ./cmd/skald

test:
	go test ./...

lint:
	go vet ./...

bench:
	go run ./cmd/skald-bench -procs 4
	go run ./cmd/skald-bench -procs 8

# Multi-process concurrency test. SKALD_CONC_RACE=1 builds the binary with
# -race; SKALD_CONC_CLI and SKALD_CONC_CYCLES scale the workload.
conc:
	go test -count=1 -timeout 600s -run TestConcurrency ./internal/conctest/

# Cross-compile and package release archives into dist/. Pass VERSION=... to
# override the git-derived version and ALLOW_DIRTY=1 for a throwaway build.
release:
	./scripts/release.sh build

# Verify dist/ (checksums, binary types, version) and smoke-test the native binary.
release-check:
	./scripts/release.sh check

# Build and install skald into $(BINDIR). The binary is copied beside the
# destination and renamed into place, so a running skald is never half-written.
install:
	@mkdir -p "$(DESTDIR)$(BINDIR)"
	go build -ldflags "-X skald/internal/cli.Version=$(INSTALL_VERSION)" -o "$(DESTDIR)$(BINDIR)/.skald.new" ./cmd/skald
	@chmod 0755 "$(DESTDIR)$(BINDIR)/.skald.new"
	@mv -f "$(DESTDIR)$(BINDIR)/.skald.new" "$(DESTDIR)$(BINDIR)/skald"
	@echo "installed skald $(INSTALL_VERSION) to $(DESTDIR)$(BINDIR)/skald"
	@case ":$$PATH:" in *":$(BINDIR):"*) ;; *) echo "note: $(BINDIR) is not on your PATH; add it to your shell profile" ;; esac

# Pull the latest source (fast-forward only, refused on a dirty tree) and
# reinstall. Use UPDATE_PULL=0 to rebuild the current checkout without pulling.
UPDATE_PULL ?= 1
update:
	@if [ "$(UPDATE_PULL)" = "1" ]; then \
		if [ -n "$$(git status --porcelain)" ]; then echo "update: the working tree has uncommitted changes; commit or stash them, or run 'make update UPDATE_PULL=0'"; exit 1; fi; \
		git pull --ff-only || { echo "update: git pull failed; fix it or run 'make update UPDATE_PULL=0'"; exit 1; }; \
	fi
	@$(MAKE) --no-print-directory install
	@echo
	@echo "Restart any running 'skald mcp' (restart Claude Code) and 'skald serve' (for a background one: skald serve stop, then skald serve -d) so they use the new binary."
	@echo "A newer database schema is applied the first time the new binary opens it; run 'skald backup' first if you want a copy."

# Remove the installed binary. Your data in ~/.skald (or $$SKALD_HOME) is kept.
uninstall:
	@if [ -e "$(DESTDIR)$(BINDIR)/skald" ]; then rm -f "$(DESTDIR)$(BINDIR)/skald" && echo "removed $(DESTDIR)$(BINDIR)/skald"; else echo "nothing to remove at $(DESTDIR)$(BINDIR)/skald"; fi
	@echo "Your data was left in $${SKALD_HOME:-$$HOME/.skald}. Delete that directory to remove all projects."
	@echo "If you registered the MCP server, also run: claude mcp remove skald"

#!/bin/sh
# Release tooling for skald. POSIX sh so `smoke` also runs in a minimal container.
#
#   scripts/release.sh build   cross-compile and package into dist/ (default)
#   scripts/release.sh check   verify dist/ and smoke-test the native binary
#   scripts/release.sh smoke BIN PLAN   run the smoke workflow against one binary
#
# Environment: VERSION (override), ALLOW_DIRTY=1 (allow an unclean tree), DIST.
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DIST=${DIST:-$ROOT/dist}
TARGETS="darwin/arm64 darwin/amd64 linux/arm64 linux/amd64"
PLAN=$ROOT/docs/plan-example.json

die() { echo "release: $*" >&2; exit 1; }
say() { echo "release: $*"; }

native_os() {
	case $(uname -s) in
	Darwin) echo darwin ;;
	Linux) echo linux ;;
	*) die "unsupported host OS $(uname -s)" ;;
	esac
}

native_arch() {
	case $(uname -m) in
	arm64 | aarch64) echo arm64 ;;
	x86_64 | amd64) echo amd64 ;;
	*) die "unsupported host arch $(uname -m)" ;;
	esac
}

# ---------------------------------------------------------------- build

# touch -t stamp for an epoch, on both BSD and GNU date.
stamp() { date -u -r "$1" +%Y%m%d%H%M.%S 2>/dev/null || date -u -d "@$1" +%Y%m%d%H%M.%S; }

cmd_build() {
	cd "$ROOT"
	command -v go >/dev/null || die "go is not installed"
	dirty=
	if [ -n "$(git status --porcelain 2>/dev/null)" ]; then dirty=1; fi
	if [ -n "$dirty" ] && [ "${ALLOW_DIRTY:-}" != 1 ]; then
		die "working tree has uncommitted changes; commit them, or run with ALLOW_DIRTY=1 for a throwaway build"
	fi

	version=${VERSION:-}
	if [ -z "$version" ]; then
		version=$(git describe --tags --always --dirty 2>/dev/null) || die "cannot derive a version; set VERSION=..."
		if [ -n "$dirty" ]; then case $version in *-dirty) ;; *) version=$version-dirty ;; esac; fi
	fi
	# The version ends up in file names and a shell-safe ldflag.
	case $version in *[!A-Za-z0-9._+-]*) die "version '$version' may only contain letters, digits and . _ + -" ;; esac

	epoch=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || date +%s)}
	ts=$(stamp "$epoch")

	rm -rf "$DIST"
	mkdir -p "$DIST"
	work=$(mktemp -d)
	trap 'rm -rf "$work"' EXIT INT TERM

	if tar --version 2>/dev/null | grep -qi 'gnu tar'; then
		tar_flags="--sort=name --owner=0 --group=0 --numeric-owner --mtime=@$epoch"
	else
		tar_flags="--uid 0 --gid 0 --numeric-owner"
	fi

	for t in $TARGETS; do
		os=${t%/*}
		arch=${t#*/}
		name=skald_${version}_${os}_${arch}
		say "building $name"
		stage=$work/$name
		mkdir -p "$stage"
		CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
			-ldflags "-s -w -X skald/internal/cli.Version=$version" \
			-o "$stage/skald" ./cmd/skald
		for f in README.md LICENSE; do [ -f "$ROOT/$f" ] && cp "$ROOT/$f" "$stage/$f"; done
		find "$stage" -exec touch -t "$ts" {} +
		# gzip -n leaves the timestamp out of the header so the archive is stable.
		# shellcheck disable=SC2086
		(cd "$work" && tar $tar_flags -cf - "$name") | gzip -n -9 >"$DIST/$name.tar.gz"
	done

	(cd "$DIST" && { shasum -a 256 skald_*.tar.gz 2>/dev/null || sha256sum skald_*.tar.gz; } >SHA256SUMS)
	say "version $version"
	(cd "$DIST" && ls -l skald_*.tar.gz SHA256SUMS | awk '{printf "  %9d  %s\n", $5, $9}')
}

# ---------------------------------------------------------------- smoke

# smoke BIN PLAN: drive one binary through a plan-to-done session in a temp home.
cmd_smoke() {
	bin=${1:?usage: release.sh smoke BIN PLAN}
	plan=${2:?usage: release.sh smoke BIN PLAN}
	home=$(mktemp -d)
	trap 'rm -rf "$home"' EXIT INT TERM
	export SKALD_HOME=$home/skald
	unset SKALD_PROJECT SKALD_ACTOR || true
	cd "$home" # not inside any linked repo, so every command names the project

	step() { printf '  smoke: %s\n' "$*"; }
	run() { "$bin" "$@" >"$home/out" 2>"$home/err" || { echo "FAILED: skald $*" >&2; cat "$home/err" "$home/out" >&2; exit 1; }; }

	step init;               run init
	step project create;     run project create "Smoke" --no-repo
	step plan import;        run -p smoke plan import "$plan"
	step task start;         run -p smoke task start T-1
	step evidence add;       run -p smoke evidence add T-1 --kind commit --ref abc1234 --summary smoke
	step task done;          run -p smoke task done T-1
	step task show;          run -p smoke task show T-1 --json
	grep -q '"done"' "$home/out" || { echo "FAILED: T-1 is not done" >&2; cat "$home/out" >&2; exit 1; }
	step status;             run -p smoke status
	grep -q 'Smoke' "$home/out" || { echo "FAILED: status does not show the project" >&2; exit 1; }
	step export;             run -p smoke export
	grep -q '"format": *"skald-export"' "$home/out" || { echo "FAILED: export header missing" >&2; exit 1; }
	step doctor;             run doctor

	step "mcp initialize + tools/list"
	{
		printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"release-smoke","version":"1"}}}'
		sleep 1
		printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}' '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
		sleep 1
	} | "$bin" mcp >"$home/mcp.out" 2>"$home/mcp.err" || { echo "FAILED: skald mcp exited non-zero" >&2; cat "$home/mcp.err" >&2; exit 1; }
	# Every tool has exactly one inputSchema; each stdout line must be JSON-RPC.
	n=$(grep -o '"inputSchema"' "$home/mcp.out" | wc -l | tr -d ' ')
	[ "$n" = 18 ] || { echo "FAILED: expected 18 MCP tools, found $n" >&2; exit 1; }
	if grep -v '^{"jsonrpc":"2.0"' "$home/mcp.out" | grep -q .; then
		echo "FAILED: non-protocol output on MCP stdout" >&2; exit 1
	fi
	step "ok ($n tools)"
}

# ---------------------------------------------------------------- check

cmd_check() {
	[ -f "$DIST/SHA256SUMS" ] || die "no $DIST/SHA256SUMS; run 'make release' first"
	work=$(mktemp -d)
	trap 'rm -rf "$work"' EXIT INT TERM

	say "verifying checksums"
	(cd "$DIST" && { shasum -a 256 -c SHA256SUMS 2>/dev/null || sha256sum -c SHA256SUMS; }) >/dev/null || die "SHA256SUMS does not match the archives"

	first=$(cd "$DIST" && ls skald_*.tar.gz | head -n 1)
	version=${first#skald_}
	version=${version%_*_*.tar.gz}
	nos=$(native_os)
	narch=$(native_arch)

	for t in $TARGETS; do
		os=${t%/*}
		arch=${t#*/}
		name=skald_${version}_${os}_${arch}
		[ -f "$DIST/$name.tar.gz" ] || die "missing $name.tar.gz"
		mkdir -p "$work/$name.x"
		tar -xzf "$DIST/$name.tar.gz" -C "$work/$name.x" || die "$name.tar.gz does not extract"
		bin=$work/$name.x/$name/skald
		[ -x "$bin" ] || die "$name.tar.gz has no executable skald"
		desc=$(file "$bin")
		case $os/$arch in
		linux/arm64) want='ELF.*(ARM aarch64|aarch64)' ;;
		linux/amd64) want='ELF.*x86-64' ;;
		darwin/arm64) want='Mach-O.*arm64' ;;
		darwin/amd64) want='Mach-O.*x86_64' ;;
		esac
		echo "$desc" | grep -Eq "$want" || die "$name: wrong binary type: $desc"
		if [ "$os" = linux ]; then
			echo "$desc" | grep -Eq 'statically linked|static-pie' || die "$name is not static: $desc"
		fi
		say "ok  $name  ($(echo "$desc" | sed 's/^[^:]*: //' | cut -c1-70))"
	done

	nbin=$work/skald_${version}_${nos}_${narch}.x/skald_${version}_${nos}_${narch}/skald
	got=$("$nbin" --version)
	[ "$got" = "$version" ] || die "native binary reports version '$got', expected '$version'"
	say "native $nos/$narch binary reports version $version"

	say "smoke test: native $nos/$narch"
	cmd_smoke "$nbin" "$PLAN"

	if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
		lbin=$work/skald_${version}_linux_${narch}.x/skald_${version}_linux_${narch}/skald
		if docker image inspect alpine >/dev/null 2>&1 || docker pull -q alpine >/dev/null 2>&1; then
			say "smoke test: linux/$narch in an alpine container"
			mkdir -p "$work/ctr"
			cp "$lbin" "$work/ctr/skald"
			cp "$PLAN" "$work/ctr/plan.json"
			cp "$ROOT/scripts/release.sh" "$work/ctr/release.sh"
			docker run --rm -v "$work/ctr:/t:ro" alpine sh /t/release.sh smoke /t/skald /t/plan.json || die "container smoke test failed"
		else
			say "SKIPPED container smoke test: the alpine image is not available"
		fi
	else
		say "SKIPPED container smoke test: docker is not available"
	fi
	say "all checks passed"
}

case ${1:-build} in
build) cmd_build ;;
check) cmd_check ;;
smoke) shift; cmd_smoke "$@" ;;
*) die "unknown command '$1' (build, check, smoke)" ;;
esac

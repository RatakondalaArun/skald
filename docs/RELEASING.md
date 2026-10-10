# Releasing skald

## Make a release

```sh
git tag v0.1.0            # optional; see "Version" below
make release              # cross-compile and package into dist/
make release-check        # verify dist/ and smoke-test the result
```

`make release` refuses to run on a dirty tree (uncommitted or untracked files). For a throwaway local build use `ALLOW_DIRTY=1 make release`; the version then ends in `-dirty`.

Output in `dist/`:

- `skald_<version>_<os>_<arch>.tar.gz` for darwin and linux, each on arm64 and amd64. An archive holds one directory with `skald`, plus `README.md` and `LICENSE` when they exist.
- `SHA256SUMS`

Then publish the archives and `SHA256SUMS` (for example as GitHub release assets). The script does not upload anything.

## Version

`VERSION` is, in order:

1. the `VERSION` environment variable or `make` variable, if set (`make release VERSION=1.2.3`);
2. `git describe --tags --always --dirty`: the tag on HEAD (`v0.1.0`), `v0.1.0-3-gabc1234` when commits follow a tag, or the short hash when there are no tags.

It may contain only letters, digits and `. _ + -`, since it goes into file names. It is baked into the binary with `-X skald/internal/cli.Version`, so `skald --version` prints it.

## What the build does

- `CGO_ENABLED=0`, `-trimpath`, `-ldflags "-s -w"`. The SQLite driver is pure Go, so the Linux binaries are static and need no libc. They run in a minimal container such as Alpine.
- Archive timestamps come from the HEAD commit (`SOURCE_DATE_EPOCH` overrides), with owner 0 and no gzip header time. Two builds of the same commit give identical archives on the same toolchain. This is "reproducible-ish": it is not verified across different Go versions or machines.

## What `release-check` verifies

- `SHA256SUMS` matches the archives.
- Every archive extracts and holds an executable `skald`.
- Each binary has the right type for its target (`file`): Mach-O arm64 or x86_64, ELF aarch64 or x86-64, and the Linux binaries are statically linked.
- The native binary prints the expected `--version`.
- A smoke session on the native binary in a temporary `SKALD_HOME`: `init`, `project create`, `plan import docs/plan-example.json`, `task start`, `evidence add`, `task done`, `status`, `export`, `doctor`, then an MCP `initialize` and `tools/list` over pipes. It asserts 18 tools and that stdout carries only JSON-RPC.
- If Docker is running, the same smoke session runs the Linux binary for your CPU architecture in an Alpine container. It is skipped with a message when Docker or the `alpine` image is unavailable.

Not verified by the script: the non-native binaries are not executed (only inspected with `file`), so the macOS amd64 and Linux binaries for the other architecture are untested at runtime.

## Verify a download

```sh
shasum -a 256 -c SHA256SUMS --ignore-missing    # macOS
sha256sum -c SHA256SUMS --ignore-missing        # Linux
```

Checksums show the file is intact and matches what was published. They are not a signature, so publish `SHA256SUMS` over a channel you trust.

## macOS: unsigned binaries

The binaries are not signed or notarized. A binary downloaded through a browser gets a quarantine flag, and Gatekeeper will refuse to run it. Clear the flag after checking the checksum:

```sh
xattr -d com.apple.quarantine ./skald
```

Binaries fetched with `curl` do not get the flag. Signing and notarizing needs an Apple Developer ID and is not set up.

## Windows

Windows is out of scope. File-mode handling (`0700`/`0600` on `~/.skald`) and signal handling (SIGINT/SIGTERM shutdown of `serve`, `mcp` and the follow commands) have not been tested there, and the CI matrix does not include it.

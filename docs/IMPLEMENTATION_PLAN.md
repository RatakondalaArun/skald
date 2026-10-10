# Skald — Implementation Plan

See `ARCHITECTURE.md` for the design. Each milestone ends in something runnable and tested. Do not start a milestone before the previous one's exit criteria pass.

Order: core → CLI → MCP (the critical path) → live watch → web → hardening. Once M4 is done, track Skald's own remaining work in Skald.

## M0 — Scaffold

- `go mod init`, `.gitignore`, `Makefile` (`build`, `test`, `lint`), CI-ready `go vet` and `go test ./...`.
- Directory layout from the architecture doc, with `cmd/skald/main.go` printing a version.
- Cobra root command, `slog` setup (stderr only), `SKALD_HOME` handling.

**Exit:** `make build` produces `./skald`, and `skald --version` works.

## M1 — Store and migrations

- `internal/store`: open DB at `$SKALD_HOME` (default `~/.skald`) with correct permissions, DSN pragmas, writer pool (1 conn) and reader pool.
- `WriteTx` helper using `BEGIN IMMEDIATE`.
- Embedded migration runner using `PRAGMA user_version`. Refuse a newer schema. Migration `0001` creates every table and index from the architecture doc.
- **Driver benchmark**: 4 processes × 1,000 small write transactions each against one DB with `modernc.org/sqlite`. Record p50/p99 latency and any busy errors in `docs/BENCHMARKS.md`. Decide whether to keep the driver.

**Exit:** store tests pass. The benchmark shows zero `SQLITE_BUSY` surfaced to callers and an acceptable p99 (target under 100 ms).

## M2 — Domain and service

1. `internal/domain`: types, ID prefix parsing and formatting (`T-42`), task state machine with transition rules, derived `ready` and `stale`, progress function. Table-driven tests first.
2. `internal/store`: queries for projects, features, tasks, dependencies, artifacts, documents and events. Every mutation appends its event in the same transaction.
3. `internal/service`:
   - Project resolution (explicit, env, cwd walk-up, single active project).
   - `CreateProject`, `CreateFeature`, `CreateTask`, `CreatePlan` (bulk, idempotent, cycle-checked), `StartTask`, `UpdateStatus`, `AddNote`, `AddDependency` (cycle check), `RecordArtifact`, `CompleteTask` (evidence gate), `NextTasks`, `ProjectProgress`, `EventsSince`, documents with versions.
   - Typed errors with next-step messages.
   - `expected_version` conflict handling.

**Exit:** a service-level test creates a plan, works every task through to done in dependency order, and the final progress is 100%. Cycle, evidence and conflict cases all return the right typed error.

## M3 — CLI

Commands (all call the service layer; `--json` on every read command):

- `skald init` (create `~/.skald`, run migrations)
- `skald project create|list|show|archive` (default `repo_path` is the cwd)
- `skald feature add|list`
- `skald task add|list|show|start|done|block|unblock|cancel|reopen|note|dep`
- `skald next` (ready tasks)
- `skald evidence add <task> --kind commit --ref <sha>`
- `skald status` (progress tree, blocked list, ready list)
- `skald log [--follow]` (events)
- `skald backup`

Plain, aligned text output. Exit codes: 0 ok, 1 error, 2 usage.

**Exit:** a human can plan and complete a small project using only the CLI. CLI integration tests run the built binary against a temp `SKALD_HOME`.

## M4 — MCP server (critical path)

1. `skald mcp`: stdio server using the MCP Go SDK. stdout is protocol only. A test asserts that no stray bytes appear on stdout.
2. Map every tool in the architecture doc onto a service call. Include actor from the `initialize` client info.
3. Compact responses, list caps with truncation notices, service errors passed through verbatim.
4. Short tool descriptions that teach the workflow, plus the optional workflow prompt.
5. Test with an in-process MCP client running a full scripted session: `create_project` → `create_plan` → loop of `get_next_tasks` / `start_task` / `add_task_note` / `record_verification` / `report_task_result` → `get_project_progress`.
6. Register with Claude Code (`claude mcp add skald -- skald mcp`) and run a real planning session on a toy repo. Fix the descriptions and error messages that confuse the model.
7. **Start dogfooding**: create a Skald project in Skald, import the remaining milestones as features and tasks.

**Exit:** Claude Code can plan a project, decompose it, execute tasks and complete them, while the CLI shows the same state, with no manual database edits. The agent cannot complete a task without evidence under the default settings.

## M5 — Change feed and `status --watch`

- `internal/watch`: `data_version` polling with an event cursor, and a clean shutdown on context cancel.
- `skald log --follow` and `skald status --watch` (redraw on change, ANSI only, no TUI framework).
- Tests: events written by a separate process arrive within 1 second, in order, with no duplicates.

**Exit:** with `skald status --watch` open, Claude Code's MCP calls appear live.

## M6 — Web dashboard

1. `internal/web`: JSON API (read endpoints plus unblock/cancel/reopen), reusing the service layer. Typed errors map to HTTP status codes.
2. Security: loopback bind, `Host` and `Origin` checks, per-process token on mutating endpoints, refusal of non-loopback `--addr` without a flag.
3. SSE endpoint `/events` backed by the watcher, with `Last-Event-ID` replay.
4. Embedded static UI (`go:embed`): projects list, project overview with board, task detail, activity feed. Vanilla JS or htmx, no build step.
5. `skald serve [--addr 127.0.0.1:7331] [--open]`.
6. Tests: handler tests, SSE reconnect replay, rejection of bad `Host`/`Origin`.

**Exit:** the dashboard updates live while Claude works, and a request with a forged `Origin` is rejected.

## M7 — Hardening and release

- Multi-process concurrency test (several `skald` processes plus MCP sessions plus a server) in CI.
- Migration test from every prior schema version. Stale-binary refusal test.
- Error recovery: corrupted-DB message pointing at `skald backup` files, lock-timeout messages that say what is holding up.
- `skald export --json` for escape-hatch data portability.
- Cross-compiled builds with `GOOS`/`GOARCH` for darwin and linux (arm64, amd64), plus windows if desired. Release script and `README.md` quick start (install, `claude mcp add`, first plan).
- Review the MCP tool descriptions once more using transcripts from real sessions.

**Exit:** a clean install on a second machine goes from download to a tracked Claude Code session using only the README.

## Dependencies between milestones

```
M0 → M1 → M2 → M3 → M4 ──→ M7
                    └→ M5 → M6 ─┘
```

M5 and M6 can be done in either order after M4, but M5 is small and M6 builds on it.

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| `modernc.org/sqlite` write contention under several processes | M1 benchmark gate. Swap driver behind `database/sql` if it fails |
| MCP Go SDK API churn | Keep all SDK usage inside `internal/mcpserver`. Fallback to `mcp-go` |
| The agent misuses tools (skips evidence, duplicates tasks) | Evidence gate, idempotency keys, clear errors, real-session tuning in M4 |
| Stale long-lived `skald mcp` after upgrade | Schema version refusal with a clear "restart Claude Code / upgrade" message |
| Scope creep into a second UI | Web is read-mostly. `status --watch` stands in for a TUI. Revisit only after M7 |
| Wrong project resolved for an MCP session | Deterministic resolution order, plus the project slug echoed in every MCP response |

## Definition of done (v1)

- Claude Code can create a project plan, execute it and report completion with evidence, entirely through MCP.
- CLI and web dashboard show identical, live state.
- Progress is always derived. No agent can set it.
- Concurrent CLI, MCP and server processes never corrupt or lose data.
- One binary, one database under `~/.skald`, installable from the README alone.

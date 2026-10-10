# Skald — Features and Tasks

Derived from `IMPLEMENTATION_PLAN.md`. Each feature is a milestone. Each task has a ref, a weight (1–10, default 1), dependencies and acceptance criteria. Refs are local to this file and map to the `ref` names accepted by `create_plan` (see `ARCHITECTURE.md` section 7), so this file can be imported into Skald once M4 exists.

Feature weights reflect relative effort. Dependencies between features are expressed through task dependencies.

---

## F0 — Scaffold (weight 1)

| Ref | Task | W | Depends on | Acceptance criteria |
|---|---|---|---|---|
| f0.mod | Initialise Go module, `.gitignore`, directory layout | 1 | none | Layout matches the architecture doc, `go build ./...` passes |
| f0.make | Makefile with `build`, `test`, `lint`; `go vet` clean | 1 | f0.mod | `make build test` succeeds on a clean checkout |
| f0.cli | Cobra root command, `--version`, `slog` to stderr, `SKALD_HOME` handling | 1 | f0.mod | `skald --version` prints a version and nothing is logged to stdout |

## F1 — Store and migrations (weight 3)

| Ref | Task | W | Depends on | Acceptance criteria |
|---|---|---|---|---|
| f1.open | Open DB under `SKALD_HOME` (default `~/.skald`) with `0700`/`0600` modes and DSN pragmas | 2 | f0.cli | WAL, busy timeout and foreign keys verified by a test on every pooled connection |
| f1.pools | Writer pool (1 conn) and reader pool, `WriteTx` using `BEGIN IMMEDIATE` | 2 | f1.open | Concurrent writers in one process never get `SQLITE_BUSY` |
| f1.migrate | Embedded migration runner on `PRAGMA user_version`, refuse newer schema | 2 | f1.open | Test: opening a DB with a higher version returns a clear upgrade error |
| f1.schema | Migration `0001`: all tables, constraints, indexes | 3 | f1.migrate | Constraint tests: weight and priority ranges, self-dependency, foreign keys |
| f1.bench | Multi-process write benchmark and driver decision, recorded in `docs/BENCHMARKS.md` | 3 | f1.pools, f1.schema | 4 processes × 1,000 writes: no surfaced busy errors, p99 under 100 ms, or a driver-swap decision documented |

## F2 — Domain and service (weight 8)

| Ref | Task | W | Depends on | Acceptance criteria |
|---|---|---|---|---|
| f2.types | Domain types and short-ID parse/format (`P-`, `F-`, `T-`) | 1 | f0.mod | Round-trip tests, rejects malformed or wrong-prefix IDs |
| f2.fsm | Task state machine, derived `ready` and `stale` | 3 | f2.types | Table-driven test covers every legal and illegal transition |
| f2.progress | Progress function: task and feature weights, cancelled excluded | 2 | f2.types | Tests for empty features, all cancelled, mixed weights |
| f2.queries | Store queries for projects, features, tasks, deps, artifacts, documents, events | 5 | f1.schema, f2.types | Every mutation writes its event in the same transaction (tested by forcing a rollback) |
| f2.resolve | Project resolution: explicit, env, cwd walk-up, single active | 2 | f2.queries | Tests for each step and for the ambiguous case listing candidates |
| f2.errors | Typed errors with next-step messages | 2 | f2.types | Each error type has a message test that names a corrective action |
| f2.plan | `CreatePlan`: bulk, one transaction, idempotent, cycle-checked | 5 | f2.queries, f2.errors | Re-running the same plan creates nothing new. A cycle rolls back the whole plan |
| f2.lifecycle | Start, update status, block/unblock, cancel/reopen, notes | 4 | f2.fsm, f2.queries, f2.errors | Dependency-gated start returns the unmet list. `expected_version` conflict returns current state |
| f2.evidence | Artifacts and the evidence gate on completion, with `--force` override recorded | 3 | f2.lifecycle | Completion without evidence fails by default. Forced completion is visible in the event |
| f2.next | `NextTasks` (ready, ordered by priority then position) and `ProjectProgress` | 2 | f2.progress, f2.lifecycle | Ordering and readiness tests with diamond-shaped dependencies |
| f2.docs | Versioned documents | 2 | f2.queries | Each put creates a new version, old versions are readable |
| f2.e2e | Service-level end-to-end test: plan → complete all tasks → 100% | 2 | f2.next, f2.evidence, f2.plan | Passes, and the event log replays the full history in order |

## F3 — CLI (weight 5)

| Ref | Task | W | Depends on | Acceptance criteria |
|---|---|---|---|---|
| f3.init | `skald init`, `project create/list/show/archive` | 2 | f2.resolve, f2.lifecycle | `repo_path` defaults to the cwd. Re-running `init` is safe |
| f3.plan | `feature add/list`, `task add/list/show` | 2 | f3.init | `--json` output on all read commands |
| f3.work | `task start/done/block/unblock/cancel/reopen/note/dep`, `evidence add` | 3 | f3.plan, f2.evidence | Errors print the service's next-step message. Exit codes 0/1/2 |
| f3.view | `next`, `status`, `log [--follow]` | 2 | f3.plan, f2.next | `status` shows progress tree, blocked list and ready list |
| f3.backup | `backup` using `VACUUM INTO` | 1 | f1.open | Backup opens and passes `PRAGMA integrity_check` |
| f3.itest | CLI integration tests on the built binary with a temp `SKALD_HOME` | 3 | f3.work, f3.view | A scripted plan-to-done session passes |

## F4 — MCP server (weight 8)

| Ref | Task | W | Depends on | Acceptance criteria |
|---|---|---|---|---|
| f4.server | `skald mcp` over stdio with the MCP Go SDK, actor from client info | 3 | f2.resolve, f2.lifecycle | A test asserts only protocol JSON on stdout. Logs go to stderr |
| f4.read | Read tools: `get_project`, `get_project_progress`, `list_features`, `list_tasks`, `get_task`, `get_next_tasks`, `get_document` | 3 | f4.server, f2.next | Lists capped at 50 with a truncation notice. Project slug echoed |
| f4.write | Write tools: `create_project`, `create_feature`, `create_task`, `add_task_dependency`, `put_document` | 3 | f4.server | Errors pass through verbatim from the service |
| f4.plan | `create_plan` tool | 2 | f4.server, f2.plan | Idempotent through `idempotency_key`. Local refs resolve dependencies |
| f4.work | Work tools: `start_task`, `update_task_status`, `add_task_note`, `record_verification`, `report_task_result` | 4 | f4.server, f2.evidence | `report_task_result` adds note, artifacts and completes in one transaction |
| f4.prompt | Workflow prompt and tuned tool descriptions | 1 | f4.read, f4.write, f4.work | Descriptions are short and name the next tool to call |
| f4.itest | In-process MCP client scripted session: plan → loop → progress | 3 | f4.plan, f4.work, f4.read | Full session passes. Agent cannot complete without evidence under default settings |
| f4.real | Real Claude Code session on a toy repo (`claude mcp add skald -- skald mcp`), tune from transcript | 3 | f4.itest, f4.prompt | Claude plans, executes and completes tasks with no manual DB edits. Confusing responses fixed |
| f4.dogfood | Import this file into Skald and track the remaining work in it | 1 | f4.real | Skald shows the remaining features and tasks of this plan |

## F5 — Change feed and `status --watch` (weight 2)

| Ref | Task | W | Depends on | Acceptance criteria |
|---|---|---|---|---|
| f5.watch | `internal/watch`: `data_version` polling with an event cursor, clean shutdown | 3 | f2.queries | Events from another process arrive within 1 s, in order, with no duplicates |
| f5.follow | `log --follow` and `status --watch` (ANSI redraw) | 2 | f5.watch, f3.view | Live updates visible while MCP calls run. Ctrl-C exits cleanly |

## F6 — Web dashboard (weight 5)

| Ref | Task | W | Depends on | Acceptance criteria |
|---|---|---|---|---|
| f6.api | JSON API: read endpoints plus unblock/cancel/reopen, error-to-status mapping | 3 | f2.lifecycle, f2.next | Handler tests for each endpoint and error type |
| f6.sec | Loopback bind, `Host`/`Origin` checks, mutation token, non-loopback refusal | 3 | f6.api | Forged `Host` or `Origin` rejected. `--addr 0.0.0.0` refused without the flag |
| f6.sse | SSE endpoint with `Last-Event-ID` replay | 3 | f5.watch, f6.api | Reconnect test receives the missed events exactly once |
| f6.ui | Embedded static UI: projects, overview board, task detail, activity feed | 5 | f6.api, f6.sse | Updates live while MCP writes. No build step |
| f6.serve | `skald serve [--addr] [--open]` | 1 | f6.sec, f6.ui | Serves on `127.0.0.1:7331` by default |

## F7 — Hardening and release (weight 3)

| Ref | Task | W | Depends on | Acceptance criteria |
|---|---|---|---|---|
| f7.conc | CI multi-process concurrency test: CLIs, MCP sessions and server together | 3 | f4.itest, f6.serve | No lost or duplicated events, no surfaced busy errors |
| f7.mig | Migration test from every prior schema version, stale-binary refusal test | 2 | f1.migrate | All upgrade paths pass |
| f7.recover | Error recovery messages for corruption and lock timeouts | 2 | f3.backup | Messages name the backup directory and the recovery command |
| f7.export | `skald export --json` | 1 | f2.queries | Export round-trips the project data |
| f7.build | Cross-compiled releases (darwin/linux, arm64/amd64), release script | 2 | f7.conc | Binaries run on a clean machine |
| f7.readme | README quick start: install, `claude mcp add`, first plan | 1 | f7.build | A clean install reaches a tracked Claude session using only the README |
| f7.tune | Final MCP description review from real transcripts | 1 | f4.real | Changes recorded in the commit message or `docs/` |

---

## Summary

| Feature | Weight | Tasks |
|---|---|---|
| F0 Scaffold | 1 | 3 |
| F1 Store and migrations | 3 | 5 |
| F2 Domain and service | 8 | 12 |
| F3 CLI | 5 | 6 |
| F4 MCP server | 8 | 9 |
| F5 Change feed and watch | 2 | 2 |
| F6 Web dashboard | 5 | 5 |
| F7 Hardening and release | 3 | 7 |
| **Total** | **35** | **49** |

Critical path: f0.cli → f1.open → f1.migrate → f1.schema → f2.queries → f2.plan / f2.lifecycle → f2.evidence → f4.server → f4.work → f4.itest → f4.real → f7.

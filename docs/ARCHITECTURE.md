# Skald — Architecture

Skald is a local-first project tracker for coding agents. Claude Code plans a project, splits it into features and tasks, and reports progress through MCP. A human watches through a CLI and a small web dashboard. There is no agent runtime, no orchestration engine and no cloud backend.

## 1. Principles

1. **SQLite is the only source of truth.** Every entry point reads and writes the same database. Nothing keeps task state in memory.
2. **One domain core, thin entry points.** The CLI, MCP server and web server call the same service layer. Business rules live there and nowhere else.
3. **Agents report facts, the system computes conclusions.** Progress, "ready" and "blocked" are derived from task state. An agent cannot set a percentage.
4. **Design for LLM callers.** Short IDs, compact output, actionable error messages, bulk operations and idempotent writes.
5. **Local only.** Binds to loopback, no accounts, no network dependencies. The one exception is GitHub sync (section 13), which talks to GitHub only when a user or agent runs it.

## 2. Process model

One Go binary, `skald`, with subcommands. Processes are independent and share `~/.skald/skald.db`.

| Command | Role |
|---|---|
| `skald init` / `project` / `feature` / `task` / `status` | CLI for humans and scripts |
| `skald status --watch` | Live terminal view (replaces a full TUI) |
| `skald mcp` | stdio MCP server, launched by Claude Code. One process per Claude session |
| `skald serve` | HTTP API, embedded web dashboard, SSE |

```
 CLI ─────┐
 MCP ─────┼──> service layer ──> store (sqlite) ──> ~/.skald/skald.db (WAL)
 web API ─┘                                              ^
                                                         │ polls event cursor
 SSE / status --watch ───────────────────────────────────┘
```

Several `skald mcp` processes (one per Claude Code session), a `skald serve` and CLI invocations can all be live at once. SQLite in WAL mode handles that, with the rules in section 5.

## 3. Storage location and project resolution

- Database: `$HOME/.skald/skald.db`. Override with `SKALD_HOME`, which tests use. Directory mode `0700`, file mode `0600`.
- Backups: `skald backup` runs `VACUUM INTO ~/.skald/backups/skald-<timestamp>.db`.
- A **project** is linked to a repository by `repo_path`, stored as an absolute path with symlinks resolved.

**How a command or MCP session picks its project**, in order:
1. An explicit `project` argument (slug).
2. `SKALD_PROJECT` environment variable.
3. The current working directory, walking up parents until a project's `repo_path` matches.
4. If exactly one active project exists, use it.
5. Otherwise return an error that lists the candidate slugs.

Claude Code launches the MCP server with the repo as its working directory, so step 3 normally resolves without any configuration. MCP tools also accept an optional `project` argument.

Registration in a repo: `claude mcp add skald -- skald mcp`.

## 4. Data model

All timestamps are UTC, stored as ISO-8601 text (`2026-10-10T12:00:00Z`). Foreign keys are enforced. IDs are global autoincrement integers. The UI, CLI and MCP show them with a prefix: `P-1`, `F-7`, `T-42`. Short IDs are cheaper in tokens and harder for an agent to mistype than UUIDs. The prefix is parsed back to an integer at the edge, so the database stores plain integers.

### Tables

```sql
projects(
  id INTEGER PRIMARY KEY, slug TEXT UNIQUE NOT NULL, name TEXT NOT NULL,
  description TEXT, status TEXT NOT NULL DEFAULT 'active',   -- active|archived
  repo_path TEXT UNIQUE,
  settings TEXT NOT NULL DEFAULT '{}',                       -- JSON, e.g. {"require_evidence": true}
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL)

features(
  id INTEGER PRIMARY KEY, project_id INTEGER NOT NULL REFERENCES projects,
  title TEXT NOT NULL, description TEXT,
  status TEXT NOT NULL DEFAULT 'planned',                    -- planned|active|done|cancelled (see 4.2)
  weight INTEGER NOT NULL DEFAULT 1 CHECK (weight BETWEEN 1 AND 10),
  position INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL)

tasks(
  id INTEGER PRIMARY KEY, project_id INTEGER NOT NULL REFERENCES projects,
  feature_id INTEGER NOT NULL REFERENCES features,
  title TEXT NOT NULL, description TEXT, acceptance_criteria TEXT,
  status TEXT NOT NULL DEFAULT 'todo',                       -- todo|in_progress|blocked|done|cancelled
  priority INTEGER NOT NULL DEFAULT 2 CHECK (priority BETWEEN 0 AND 3),  -- 0 = urgent
  weight INTEGER NOT NULL DEFAULT 1 CHECK (weight BETWEEN 1 AND 10),
  position INTEGER NOT NULL DEFAULT 0,
  assignee TEXT,                                             -- actor that started it
  blocked_reason TEXT,
  idempotency_key TEXT,                                      -- UNIQUE(project_id, idempotency_key)
  tags TEXT NOT NULL DEFAULT '[]',                           -- JSON array, normalized and sorted (migration 0002)
  version INTEGER NOT NULL DEFAULT 1,                        -- optimistic concurrency
  started_at TEXT, completed_at TEXT,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL)

task_dependencies(
  task_id INTEGER NOT NULL REFERENCES tasks ON DELETE CASCADE,
  depends_on_id INTEGER NOT NULL REFERENCES tasks ON DELETE CASCADE,
  PRIMARY KEY (task_id, depends_on_id), CHECK (task_id <> depends_on_id))

events(                                                      -- append-only
  id INTEGER PRIMARY KEY AUTOINCREMENT,                      -- the change cursor
  project_id INTEGER NOT NULL, entity_type TEXT NOT NULL,    -- project|feature|task|artifact|document
  entity_id INTEGER NOT NULL, type TEXT NOT NULL,            -- task.status_changed, task.note, ...
  actor TEXT NOT NULL,                                       -- human | claude:<session> | ...
  payload TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL)

artifacts(                                                   -- verification evidence
  id INTEGER PRIMARY KEY, task_id INTEGER NOT NULL REFERENCES tasks ON DELETE CASCADE,
  kind TEXT NOT NULL,                                        -- commit|pr|test_report|link|manual
  ref TEXT NOT NULL, summary TEXT, actor TEXT NOT NULL, created_at TEXT NOT NULL)

documents(                                                   -- requirements, architecture, decisions, conventions
  id INTEGER PRIMARY KEY, project_id INTEGER NOT NULL REFERENCES projects,
  kind TEXT NOT NULL, title TEXT NOT NULL, current_version INTEGER NOT NULL,
  UNIQUE(project_id, kind, title))
document_versions(
  document_id INTEGER NOT NULL REFERENCES documents, version INTEGER NOT NULL,
  body TEXT NOT NULL, actor TEXT NOT NULL, created_at TEXT NOT NULL,
  PRIMARY KEY (document_id, version))
```

Indexes: `tasks(project_id, status)`, `tasks(feature_id)`, `task_dependencies(depends_on_id)`, `events(project_id, id)`, `artifacts(task_id)`.

Notes are events (`task.note`) with the text in `payload`, so there is no separate notes table. Documents are versioned because agents tend to rewrite them.

### 4.1 Task state machine

```
          start                    complete (evidence gate)
 todo ──────────────> in_progress ──────────────────────────> done
  ^  \                 |   ^                                    |
  |   \ block          |   | unblock                            | reopen
  |    └────> blocked <┘   |                                    v
  |             |──────────┘                                   todo
  └── reopen ───┘
 any non-done ──cancel──> cancelled ──reopen──> todo
```

| From | To | Rule |
|---|---|---|
| todo | in_progress | All dependencies are `done` or `cancelled`. Sets `assignee`, `started_at` |
| todo, in_progress | blocked | `blocked_reason` is required |
| blocked | in_progress, todo | Clears the reason |
| in_progress | done | Evidence gate (below). Sets `completed_at` |
| done, cancelled | todo | Reopen. Clears `completed_at` |
| any except done | cancelled | |

- **Ready** is derived: `status = todo` and every dependency is done or cancelled. It is not stored.
- **Waiting on dependencies** is also derived. Only an explicit blocker (a human or agent reporting an external obstacle) is stored as `blocked`.
- **Evidence gate**: when the project setting `require_evidence` is true (the default), `done` requires at least one artifact on the task. Humans can override with `--force`, which is recorded in the event.
- **Stale work**: an `in_progress` task untouched for 4 hours is flagged `stale` in output. There are no leases.
- **Cycles**: adding a dependency runs a cycle check inside the write transaction. Dependencies across projects are rejected.
- **Optimistic concurrency**: every task mutation accepts an optional `expected_version`. A mismatch returns a conflict error with the current state.

### 4.2 Feature status

Derived from its tasks and not set directly: `planned` (no task started), `active`, `done` (all non-cancelled tasks done), `cancelled` (explicit). It is computed on read and not stored, so the stored `status` column only holds `cancelled` or `planned`. A feature with zero tasks counts as 0% and `planned`.

### 4.3 Progress

```
feature_progress = Σ weight(done tasks) / Σ weight(non-cancelled tasks)
project_progress = Σ feature.weight × feature_progress / Σ feature.weight   (over non-cancelled features)
```

Computed by one pure function in `internal/domain` over rows from one query. It is never stored or accepted as input.

## 5. SQLite concurrency rules

Driver: `modernc.org/sqlite` (no CGO, easy cross-compilation). Benchmark it in milestone M1. If write latency under contention is unacceptable, swap to `mattn/go-sqlite3` behind the same `database/sql` interface.

- DSN pragmas, applied to every connection: `journal_mode=WAL`, `busy_timeout=5000`, `foreign_keys=ON`, `synchronous=NORMAL`.
- Two pools: a **writer** with `MaxOpenConns(1)` and a **reader** pool.
- Writes run in `BEGIN IMMEDIATE` through one helper, `store.WriteTx(ctx, func(tx) error)`. A deferred transaction that upgrades to a write fails with `SQLITE_BUSY` immediately, and `busy_timeout` does not apply to that failure.
- Each mutation and its event row go in the same transaction. Transactions stay short, with no network or LLM calls inside.
- Schema version: migrations are embedded SQL files tracked with `PRAGMA user_version`. On startup, a binary **refuses** to run against a newer schema than it knows ("upgrade skald"). `skald mcp` is long-lived, so this protects against a stale MCP process after an upgrade. Migrations run under an exclusive write lock.
- The database stays on a local disk. Do not put `~/.skald` on a network filesystem.

## 6. Service layer

`internal/service` exposes one method per use case. Each takes a `context.Context` and an `Actor` and returns domain types. All entry points call these, and none touch SQL.

Examples: `CreatePlan`, `StartTask`, `CompleteTask`, `BlockTask`, `AddNote`, `AddDependency`, `RecordArtifact`, `NextTasks`, `ProjectProgress`, `EventsSince`.

Responsibilities:
- Validate input and enforce the state machine.
- Run the transaction, append events, and bump `version`.
- Resolve the project (section 3) and parse short IDs.
- Return typed errors: `NotFound`, `InvalidTransition`, `Conflict`, `DependencyCycle`, `EvidenceRequired`, `Validation`. Each carries a human-readable "what to do next" message. The MCP layer passes these through verbatim.

**Actor** identity: CLI uses `human` (override with `SKALD_ACTOR`). MCP uses `claude:<client name>/<short session id>` from the MCP `initialize` handshake.

## 7. MCP interface

Transport: stdio. Protocol messages go to stdout only. Logs go to stderr through `log/slog`. SDK: the official `modelcontextprotocol/go-sdk`. Confirm its current API in M4 and fall back to `mark3labs/mcp-go` if it is not ready.

Responses are compact: short text or terse JSON, with no decorative output. Lists are capped (default 50) and say when they are truncated.

| Tool | Purpose |
|---|---|
| `create_project` | Create a project, linked to the current repo by default |
| `get_project` | Project summary: description, progress, counts, features, blockers |
| `get_project_progress` | Derived progress per feature and overall |
| `create_plan` | **Bulk**: features, tasks and dependencies in one transaction. Dependencies refer to tasks by local `ref` names inside the payload. Idempotent through `idempotency_key` per task |
| `create_feature` / `list_features` | Single-item management |
| `create_task` / `get_task` / `list_tasks` | Single-item management. `list_tasks` filters by feature, status, tags, `ready` |
| `get_next_tasks` | Ready tasks ordered by priority, then position. The "what should I do now" call |
| `start_task` | todo → in_progress. Fails with the unmet dependency list if not ready |
| `update_task_status` | Other transitions (block, unblock, cancel, reopen, complete) |
| `add_task_note` | Free-text progress note, stored as an event |
| `add_task_dependency` | With cycle check |
| `tag_task` / `list_tags` | Add or remove task tags; tags in use with task counts |
| `record_verification` | Attach an artifact: commit, pr, test_report, link, manual |
| `report_task_result` | Convenience: summary note + artifacts + complete in one call |
| `get_document` / `put_document` | Requirements, architecture, conventions. Versioned |

`update_task_status` and `start_task` overlap on purpose: `start_task` is the obvious happy-path call for the model, and `update_task_status` covers everything else.

Also expose a short MCP **prompt** or resource describing the recommended workflow (plan, get next task, start, note, record verification, complete). Keep the instruction text in the tool descriptions short.

## 8. Change notification

The `events.id` autoincrement column is the cursor. Both consumers poll the database. Nothing depends on in-memory pub/sub.

- A watcher goroutine keeps one long-lived connection and runs `PRAGMA data_version` every 300 ms. `data_version` changes only when a **different** connection commits, so it is a cheap "anything changed" check. On change it runs `SELECT ... FROM events WHERE id > :cursor`.
- `skald serve` fans new events out to browsers over **SSE**. A reconnecting client sends `Last-Event-ID` and gets the missed events replayed from the table.
- `skald status --watch` runs the same watcher in-process and redraws. It does not need `skald serve` to be running.

## 9. Web dashboard (simple)

No frontend build chain. The server is `net/http` with a JSON API and static assets embedded using `go:embed`. The UI is plain HTML with a small amount of vanilla JS (or htmx), and uses SSE for live updates.

Views:
1. **Projects** list with progress bars.
2. **Project overview**: overall progress, features with progress, a task board (todo / in progress / blocked / done), and a ready-next list.
3. **Task detail**: description, acceptance criteria, dependencies, artifacts, notes and event history.
4. **Activity feed**: live, filterable by actor.

The dashboard is **read-mostly**. From the task drawer a human can start, block, unblock, complete (with the evidence gate, and a recorded override), cancel and reopen a task, and add notes and evidence. It can also edit a task's title, description, criteria, priority and weight, and add or remove its dependencies. Plans and features stay in the CLI and MCP. Every change goes through the same service-layer rules as the CLI, so the frontend stays thin.

**Local security**: bind `127.0.0.1` by default. Reject requests whose `Host` is not the configured loopback host (DNS rebinding). Mutating endpoints require a per-process random token, delivered in the served HTML, and a matching `Origin` header. Refuse non-loopback `--addr` unless `--insecure-listen` is passed.

## 10. Repository layout

```
skald/
├── cmd/skald/main.go
├── internal/
│   ├── domain/        # types, state machine, progress (pure functions, no I/O)
│   ├── service/       # use cases, transactions, errors, project resolution
│   ├── store/         # sqlite: connection setup, WriteTx, queries, migrations/
│   ├── watch/         # event cursor poller
│   ├── cli/           # Cobra commands, output formatting
│   ├── mcpserver/     # tools -> service
│   ├── web/           # HTTP API, SSE, embedded static/
│   └── daemon/        # detached background process, pid/state file
├── docs/
├── go.mod
└── README.md
```

Dependencies are one-directional: `cli`, `mcpserver` and `web` depend on `service`, `service` on `domain`, `store` and `github`, and `domain` on nothing. `github` knows nothing about the database.

Libraries: Cobra (CLI), `modernc.org/sqlite`, the MCP Go SDK, `log/slog`, and the standard library for HTTP. Use plain `database/sql` with hand-written queries. A repository layer this small does not justify sqlc.

## 11. Testing strategy

- **Domain**: table-driven tests for every state-machine transition and the progress function, including weights, cancelled tasks and empty features.
- **Store/service**: tests against a real temp-dir SQLite. Cover cycle detection, evidence gate, idempotent `create_plan`, version conflicts and event-per-mutation.
- **Concurrency**: spawn several real `skald` processes doing concurrent writes against one DB. Assert there are no `SQLITE_BUSY` errors and that the event sequence has no gaps or duplicates.
- **MCP**: an in-process client against the server using full planning workflows. Also assert that **nothing but protocol JSON reaches stdout**.
- **Web**: HTTP handler tests, plus an SSE reconnect and replay test.
- **Migrations**: apply every migration to a fixture DB from each previous version.

## 12. Decisions deliberately deferred

- Multi-user or remote access. Out of scope.
- Editing tasks heavily from the web UI.
- Sub-tasks (the model is project → feature → task).
- Importing from or exporting to trackers other than GitHub (GitHub is section 13).
- A full TUI. `status --watch` first, and revisit only if it proves insufficient.

## 13. GitHub sync

`skald github sync`, the `sync_github` MCP tool and `POST /api/projects/{slug}/github/sync` all call `service.SyncGitHub`.

**Link.** A project's repository lives in `projects.settings` as `{"github": {"repo": "owner/name", "milestones": {"<number>": <feature id>}}}`. The milestone map is the sync's memory of which feature stands for which milestone (`"0"` is "No milestone"). Issues need no table: the task that mirrors issue 12 carries the idempotency key `github:owner/name#12` (owner/name lower-cased), so the existing `UNIQUE(project_id, idempotency_key)` makes the pull idempotent. No migration was needed, and export/import carry the link because settings are exported.

**Order.** Everything is read from GitHub (GraphQL: milestones, issues with their closing pull requests and Projects board Status, discussions) before the write transaction starts, so no network call happens inside it. Then, in one `WriteTx`:

1. A feature for every milestone that has issues (milestones without issues would count as empty 0% features). Renamed milestones rename their feature.
2. Each issue under its own `SAVEPOINT`. A domain error (for example, a closed issue whose task has unfinished dependencies) rolls back that issue only and is reported as skipped.
3. The discussions document gets a new version only when its text changed.
4. Settings are written only when the link or milestone map changed.

A dry run does the same work and rolls the transaction back.

**Rules.** Status changes go through `transitionTx`, so the state machine, dependency check and evidence gate apply as for any caller. A closed issue's task is completed only after evidence exists: merged closing PRs as `pr` artifacts, otherwise a `link` to the closed issue. `Force` is never used. GitHub never reopens a task; a task done or cancelled in Skald whose issue is open is reported as diverged, and `--push` closes the issue (REST, after the transaction commits) and records a note on the task. After the issues, `--push` closes each open milestone whose feature's derived status is done, but only when every issue of that milestone that was open on GitHub got closed by this push. A blocker set from the board is cleared by a later sync; a blocker someone reported in Skald is left alone.

**Creating issues.** `--create-issues` runs after the pull commits and before `--push`. For each feature that is not cancelled and has tasks with no issue, it creates a milestone if the feature has none, then one issue per task. A task has an issue when its idempotency key starts with `github:`, or when `settings.github.issues` maps its ID to an issue number (keys are left alone, since plans use them). Every number GitHub returns is saved in its own short write transaction before the next call, so a failure part-way loses nothing and a re-run duplicates nothing. The issue body is the task description plus a hidden `<!-- skald:T-n -->` trailer, which a pull strips. A pull resolves an issue through `settings.github.issues` before it looks up the key.

**Auth.** `GITHUB_TOKEN`, then `GH_TOKEN`, then the token saved by `skald github login`, then `gh auth token`. `SKALD_GITHUB_API_URL` points at GitHub Enterprise. `skald github login` and the dashboard's sign-in run GitHub's OAuth device flow (`internal/github/oauth.go`) against an OAuth App whose client ID is `DefaultClientID` or `SKALD_GITHUB_CLIENT_ID`; the device flow has no client secret. The token is written to `$SKALD_HOME/github.json` with mode 0600 and is never sent to the page: `GET /api/github/auth` reports only where the token comes from and who signed in. The dashboard finishes a sign-in in a background goroutine while the page polls.

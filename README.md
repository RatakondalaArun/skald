# Skald

Skald is a local-first project tracker for coding agents. Claude Code plans a project, splits it into features and tasks, and reports progress through MCP. You watch from the CLI or a web dashboard.

- One Go binary, `skald`. No cloud, no accounts, no agent runtime. GitHub sync is opt-in and runs only when you ask for it.
- State lives in a single SQLite database at `~/.skald/skald.db`. Set `SKALD_HOME` to use another directory.
- The CLI, the MCP server and the dashboard all use the same database and the same rules, so they always agree.
- Progress is computed from task state and weights. Nobody, including an agent, can set a percentage.

> **Status:** the MCP server is tested with in-process and pipe clients, and the tests drive full plan-to-done sessions. It has also been driven by two real headless Claude Code sessions (`claude -p`): one planned and built a small Python CLI to 100% with evidence on every task, the other deliberately hit the dependency, evidence and cycle errors and recovered using only the error text. Both ran on a temporary data directory. Interactive use over long sessions is not yet covered.

## Install

**From a release archive.** Archives are named `skald_<version>_<os>_<arch>.tar.gz`, with a `SHA256SUMS` file next to them. See [docs/RELEASING.md](docs/RELEASING.md) for how they are built.

```sh
grep skald_<version>_<os>_<arch>.tar.gz SHA256SUMS | shasum -a 256 -c -
tar xzf skald_<version>_<os>_<arch>.tar.gz
install -m 755 skald /usr/local/bin/skald
```

On macOS a downloaded binary may be quarantined. If it will not run, clear the flag: `xattr -d com.apple.quarantine /usr/local/bin/skald`.

**From source** (needs Go):

```sh
make build        # writes ./skald
make install      # builds and installs to ~/.local/bin/skald (no sudo)
make update       # git pull --ff-only, then reinstall
make uninstall    # removes the binary; your data in ~/.skald is kept
```

`make install` puts the binary in `$(PREFIX)/bin`, with `PREFIX` defaulting to `~/.local`. For a system-wide install use `sudo make install PREFIX=/usr/local`, and pass the same `PREFIX` to `update` and `uninstall`. Make sure the bin directory is on your `PATH`. `make update` refuses a dirty working tree (use `make update UPDATE_PULL=0` to rebuild the current checkout), and afterwards you should restart any running `skald mcp` (restart Claude Code) and `skald serve`. `make uninstall` never deletes `~/.skald`; remove that directory yourself to delete all projects, and run `claude mcp remove skald` if you registered the MCP server.

`go install` does not work: the module path is `skald`, not a fetchable import path.

## Quick start

```sh
skald init                              # create ~/.skald and the database (safe to repeat)
skald project create "Demo App"         # linked to the current directory
skald plan import docs/plan-example.json
skald next                              # tasks that can start now
skald task start T-1
skald evidence add T-1 --kind commit --ref 3f2a9c1 --summary "schema merged"
skald task done T-1
skald status                            # progress, blockers, ready list
```

`skald task done T-1` without evidence fails with: `T-1 cannot be marked done without verification evidence. Record an artifact ... first`. Attach evidence, then complete the task.

Progress is weighted by task and feature weight, so it can differ from the plain done/total count (for example 30% after 1 of 3 tasks). Agents cannot set it; it is always computed from task state.

Watch it live in another terminal:

```sh
skald status --watch                    # redraws when anything changes, including MCP writes
skald log --follow                      # event stream
skald serve                             # dashboard at http://127.0.0.1:7331
```

To keep the dashboard running without a terminal, start it in the background. It returns once the dashboard is listening, logs to `~/.skald/serve.log` and records its pid and URL in `~/.skald/serve.json`. Background mode works on Linux and macOS.

```sh
skald serve --background --open         # or -d
skald serve status                      # URL, pid, log; exits 1 if none is running
skald serve stop                        # clean shutdown, killed after 5 seconds
```

### Plan files

`skald plan import` creates features, tasks and dependencies in one transaction. Nothing is written if any part is invalid. `docs/plan-example.json` is a complete example:

```json
{
  "features": [
    {
      "title": "Authentication", "weight": 3,
      "tasks": [
        { "ref": "schema", "title": "User table and migrations", "priority": 1,
          "acceptance_criteria": "Migration applies cleanly", "idempotency_key": "auth-schema" },
        { "ref": "login", "title": "Login endpoint", "depends_on": ["schema"],
          "idempotency_key": "auth-login" }
      ]
    }
  ]
}
```

- `ref` names a task inside the file, and `depends_on` lists refs. `depends_on_existing` takes IDs such as `"T-3"`.
- `weight` is 1 to 10 (default 1). `priority` is 0 (urgent) to 3 (low), default 2.
- `tags` is an optional list of labels, such as `["backend", "auth"]`.
- With an `idempotency_key` on every task, re-running the import creates nothing new. A feature is reused only when all of its tasks have keys.

## Use with Claude Code

From the repository you want to track:

```sh
skald init
skald project create "My Project"       # links the project to this directory
claude mcp add skald -- skald mcp
```

`skald mcp` runs over stdio. Protocol messages go to stdout, logs go to stderr.

**Which project does a session use?** In this order:

1. The tool's `project` argument (a slug).
2. The `SKALD_PROJECT` environment variable.
3. The nearest parent directory of the working directory that is an active project's linked repository.
4. The only active project, if there is exactly one.

Otherwise the call fails with an error listing the candidate slugs.

### Tools

IDs are short IDs (`T-42`, `F-7`). Every tool except `create_project` takes an optional `project`.

| Tool | What it does |
|---|---|
| `create_project` | Create a project, linked to the current repo |
| `get_project` | Progress, features, blocked work, documents, evidence rule |
| `get_project_progress` | Derived progress per feature, plus blocked and stale tasks |
| `create_plan` | Features, tasks and dependencies in one transaction |
| `create_feature`, `list_features` | Add or list features |
| `create_task`, `get_task`, `list_tasks` | Add, read or filter tasks by feature, status, tags or readiness (lists are capped) |
| `get_next_tasks` | Ready tasks, most urgent first |
| `start_task` | Claim a ready task |
| `update_task_status` | Block, unblock, reopen, cancel or complete |
| `add_task_note` | Record progress, a finding or a decision |
| `add_task_dependency` | Add a prerequisite (cycles are rejected) |
| `tag_task`, `list_tags` | Add or remove tags on a task; list the project's tags with counts |
| `record_verification` | Attach evidence: commit, pr, test_report, link or manual |
| `report_task_result` | Note, evidence and done in one step. Nothing is written if it fails |
| `get_document`, `put_document` | Versioned requirements, architecture, decisions, conventions |
| `sync_github` | Pull milestones, issues, merged PRs and discussions from GitHub (see below) |

There is also an MCP prompt, `skald_workflow`, that describes the loop.

### Recommended agent workflow

1. `get_project` at session start.
2. For new work: `create_plan`. Give **every task an `idempotency_key`**, so a retried call cannot duplicate tasks.
3. Loop: `get_next_tasks`, `start_task`, `add_task_note` while working, then `report_task_result` with evidence.
4. `get_project_progress` to see where things stand.

### The evidence gate

By default a task cannot be completed without at least one evidence item. Agents cannot override this: the MCP tools do not expose `force`, and the service refuses it for agent actors. A human can complete a task anyway with `skald task done T-5 --force`. The override is recorded in the task's history.

## Work alongside GitHub

Skald can mirror a GitHub repository, so issues, milestones and pull requests stay the place your team works while Claude tracks progress in Skald.

```sh
skald github login                      # sign in once with a code at github.com/login/device
skald github link acme/rocket           # or no argument: use the checkout's origin remote
skald github sync --dry-run             # show what would change, write nothing
skald github sync                       # pull
skald github sync --push                # pull, then close issues and milestones finished in Skald
skald github sync --create-issues --push   # also create issues for tasks that started in Skald
```

| GitHub | Skald |
|---|---|
| Milestone (with issues) | Feature. Issues without a milestone go to "No milestone" |
| Issue | Task titled `#12 Title`, with the idempotency key `github:owner/name#12` |
| Merged PR linked to close the issue | `pr` evidence on the task |
| Issue closed as completed / not planned | Task done / cancelled |
| Open linked PR, or board Status "In progress" / "In review" | Task started |
| Board Status "Blocked" | Task blocked (cleared again when the board moves on) |
| Labels `P0`..`P3`, `priority: high` | Task priority |
| Discussions | Read-only list in the document `github` / `Discussions` |

- Re-running is safe: nothing is created twice, and a sync with no changes on GitHub writes nothing. Titles, descriptions and features of synced tasks follow GitHub, so edit those on GitHub.
- The evidence gate still applies. A closed issue completes its task with the merged PRs as evidence, or with a `link` to the closed issue when there is none. Nothing is forced, and a task whose dependencies are unfinished is skipped and reported.
- GitHub never reopens a task. A task finished in Skald whose issue is still open is reported, and `--push` closes the issue with a comment that lists the evidence (as "not planned" for a cancelled task). `--push` then closes each open milestone whose feature is done in Skald, once none of its issues is left open. Nothing is written to GitHub without `--push`.
- Work that started in Skald: `--create-issues` creates an issue for every task that has none (cancelled tasks aside) and a milestone for every feature that has none. Each link is saved as soon as GitHub returns it, so re-running never duplicates, and the next pull treats those issues like any other. With `--push`, the new issues of tasks already done are closed with their evidence. Try it with `--dry-run` first.
- Sign in with `skald github login` (or "Sign in to GitHub" in the dashboard): Skald shows a one-time code, you enter it at github.com/login/device, and the token is saved in `~/.skald/github.json`, readable by you only. `skald github status` shows which token a sync uses and `skald github logout` forgets it. It uses Skald's own GitHub OAuth App; on GitHub Enterprise, register an OAuth App with device flow enabled there and set `SKALD_GITHUB_CLIENT_ID` to its client ID.
- The token comes from `GITHUB_TOKEN`, then `GH_TOKEN`, then `skald github login`, then the GitHub CLI (`gh auth login`). It needs read access to issues, pull requests and discussions, and write access to issues for `--push`. Board status needs the `read:project` scope (`gh auth refresh -s read:project`); without it the sync still runs and says it skipped the boards. For GitHub Enterprise set `SKALD_GITHUB_API_URL=https://<host>/api/v3`.
- Agents can run the same sync with the `sync_github` MCP tool, and the dashboard shows a "Sync from GitHub" button once a project is linked. Board cards carry the issue number (#12), the task drawer links to the issue, and the Features tab links each feature to its milestone, for synced issues and the ones `--create-issues` made alike.
- This is the only feature that uses the network, and only when you run a sync.

## CLI reference

Global flags: `--json` (JSON output on read commands), `-p, --project <slug>`. Set `SKALD_ACTOR` to change who is recorded as the actor (default `human`).

| Command | Purpose |
|---|---|
| `init` | Create the data directory and database |
| `project create\|list\|show\|archive\|link` | Manage projects (`create` takes `--slug`, `--repo`, `--no-repo`, `--description`; `link` ties an existing project to a directory, or removes the link with `--unlink`) |
| `feature add\|list` | Add features (`--weight`, `--position`, `--description`) |
| `task add\|list\|show` | Create and view tasks (`add` needs `--feature`; also `--priority`, `--weight`, `--criteria`, `--key`, `--tag`; `list` filters with `--tag`) |
| `task start\|done\|block\|unblock\|cancel\|reopen` | Move a task through its lifecycle (`block` needs `--reason`; `done` takes `--force`, `--note`) |
| `task note`, `task edit`, `task dep add\|remove` | Notes, edits (title, description, criteria, priority, weight, `--tag`/`--untag`) and dependencies |
| `task tags` | Tags used in the project, most used first |
| `plan import <file\|->` | Import a JSON plan |
| `github link [owner/name] [--unlink]`, `github sync [--repo] [--push] [--create-issues] [--dry-run]` | Link a GitHub repository and sync it (see above) |
| `github login`, `github logout`, `github status` | Sign in to GitHub, forget the saved token, show which token a sync uses |
| `evidence add <task>` | Attach evidence (`--kind`, `--ref`, `--summary`) |
| `next` | Tasks that can start now |
| `status [--watch]` | Progress, blockers, ready and stale tasks |
| `log [--follow] [--limit] [--since] [--all-projects]` | Event history |
| `serve [--addr] [--open] [--insecure-listen] [--background]` | Web dashboard (`--background`/`-d` detaches it) |
| `serve status`, `serve stop` | Check on or stop the background dashboard |
| `mcp` | MCP server on stdio |
| `backup [--out]` | Consistent database copy |
| `export [--all] [--out]`, `import <file>` | JSON export and import |
| `doctor` | Check the database and data directory |

Exit codes: 0 success, 1 runtime or domain error, 2 usage error. Errors go to stderr, and each says what to do next.

Tags are free-form labels on tasks: up to 10 per task, each up to 32 characters of lowercase letters, digits and `- _ . : /` (input is lowercased, so `UI` becomes `ui`). Filtering by several tags matches tasks that carry all of them. The dashboard shows tags on board cards, filters the board by tag, and edits them in the task drawer.

Task lifecycle: `todo` to `in_progress` (needs all dependencies done or cancelled) to `done` (needs evidence). A task can be `blocked` with a reason or `cancelled`, and done or cancelled tasks can be reopened. "Ready" and "waiting on dependencies" are derived, not stored.

## Data safety

- `skald backup` writes `~/.skald/backups/skald-<timestamp>.db` using `VACUUM INTO`. It is safe while `skald mcp` or `skald serve` is running.
- `skald export -p <slug> --out project.json` (or `--all`) writes a versioned JSON snapshot. `skald import` loads it into an **empty** database, keeping IDs and the event log. To restore elsewhere: `SKALD_HOME=/tmp/restore skald import project.json`.
- `skald doctor` checks file modes, integrity, foreign keys, schema version, WAL size and backups, and prints the next step for each problem. It never modifies the database.
- A corrupt database file is never modified. The error names the file, lists your backups, and gives the restore steps.
- A binary refuses to open a database from a newer schema. Upgrade skald and restart any running `skald mcp` or `skald serve`.
- Keep `~/.skald` on a local disk, not a network filesystem.

## Dashboard security

`skald serve` binds to `127.0.0.1:7331` and refuses any other address unless you pass `--insecure-listen`. There is no login. It rejects requests whose `Host` is not a loopback host (DNS rebinding). Changes (unblock, cancel, reopen) need a same-origin `Origin`, a per-process token and a JSON content type. The page sends a strict Content-Security-Policy. Dashboard changes are recorded as `human:web`.

## Architecture

One domain core with thin entry points. SQLite in WAL mode is the only source of truth. Every write and its event are committed in one transaction, and the `events` table is the change feed that `status --watch`, `log --follow` and the dashboard's live updates read. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), [docs/IMPLEMENTATION_PLAN.md](docs/IMPLEMENTATION_PLAN.md) and [docs/BENCHMARKS.md](docs/BENCHMARKS.md).

```
cmd/skald            entry point
internal/domain      types, state machine, progress (pure)
internal/service     use cases, transactions, errors
internal/store       SQLite, migrations, queries
internal/watch       event-cursor poller
internal/cli         commands
internal/mcpserver   MCP tools over stdio
internal/web         HTTP API, SSE, embedded dashboard
internal/github      GitHub API client used by the sync
internal/daemon      background process start, status and stop
```

## Development

```sh
make build    # build ./skald
make test     # go test ./...
make lint     # go vet ./...
make bench    # multi-process write benchmark (docs/BENCHMARKS.md)
make conc     # multi-process concurrency test
make install  # also: make update, make uninstall (see Install)
```

`make release` builds the release archives. See [docs/RELEASING.md](docs/RELEASING.md).

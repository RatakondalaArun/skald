# Benchmarks

## F1: multi-process SQLite write contention (`f1.bench`)

Question: is `modernc.org/sqlite` (pure Go, no CGO) good enough for several `skald` processes writing to one database?

**Decision criteria** (from `IMPLEMENTATION_PLAN.md` M1): zero busy errors surfaced to callers and p99 under 100 ms.

### Method

Harness: `cmd/skald-bench` (`make bench`). Not part of the `skald` CLI.

- The parent creates a temp `SKALD_HOME`, opens the DB through `internal/store` (same pragmas, pools and `WriteTx` as production) and seeds 1 project, 1 feature and 20 tasks.
- It then re-executes itself as N child processes. Each child opens its own `store.DB` and runs 1,000 write transactions through `store.WriteTx`.
- One transaction = `UPDATE tasks SET status, version = version + 1, updated_at` + `INSERT INTO events` (what a task status change does). The 20 tasks are shared across workers, so rows are contended.
- Each child also runs a reader goroutine querying every 2 ms through the reader pool.
- Latency is measured per `WriteTx` call, so it includes any wait on the write lock. It excludes process startup and `Open`.
- Pass criteria also check that the event table holds exactly one row per successful transaction.

Env: Apple M1, 8 cores, 8 GB RAM, macOS (Darwin), Go 1.27.2, `modernc.org/sqlite` v1.60.1, local SSD. Pragmas: WAL, `synchronous=NORMAL`, `busy_timeout=5000`.

### Results (3 runs each)

| Procs | Run | Transactions | Throughput (tx/s) | p50 ms | p95 ms | p99 ms | max ms | Busy errors | Other errors | Events = tx |
|---|---|---|---|---|---|---|---|---|---|---|
| 4 | 1 | 4,000 | 9,064 | 0.07 | 0.12 | 0.17 | 78.81 | 0 | 0 | yes |
| 4 | 2 | 4,000 | 9,208 | 0.07 | 0.09 | 0.15 | 3.59 | 0 | 0 | yes |
| 4 | 3 | 4,000 | 9,221 | 0.07 | 0.10 | 0.22 | 78.97 | 0 | 0 | yes |
| 8 | 1 | 8,000 | 9,546 | 0.07 | 0.09 | 0.16 | 530.14 | 0 | 0 | yes |
| 8 | 2 | 8,000 | 10,906 | 0.07 | 0.09 | 0.14 | 629.96 | 0 | 0 | yes |
| 8 | 3 | 8,000 | 10,809 | 0.07 | 0.09 | 0.14 | 178.78 | 0 | 0 | yes |

No read errors in any run.

### Reading the numbers

- p99 is 0.14–0.22 ms against a 100 ms limit. Throughput is about 9–11k tx/s, far above what an agent and a human can generate.
- Max latency is the one thing to watch: single transactions occasionally wait 80–630 ms at the 4- and 8-process level. These waits happen when all processes start writing at the same moment, and a writer has to wait for the lock (up to the 5 s `busy_timeout`). They did not surface as errors. Real use has far lower write rates, so I don't expect this to matter, but it is why `busy_timeout` must stay at 5 s or higher.
- `synchronous=NORMAL` in WAL mode does not fsync on every commit, so these numbers do not include fsync cost. A power loss (not a process crash) can lose the most recent commits, but cannot corrupt the database. That trade-off is accepted for this tool.
- This benchmark is a stress test. Real traffic is a few writes per second.

### Decision

**Keep `modernc.org/sqlite`.** Zero busy errors across 6 runs and 36,000 transactions, p99 well under 100 ms, and no CGO. Revisit only if a later stress test (M7) shows surfaced busy errors.

Related finding from the store work: concurrent first-time `Open` of a brand-new database can hit `SQLITE_BUSY` on the one-time switch to WAL mode, because `busy_timeout` does not always apply there. The store retries for up to 10 s around migration. This is covered by a store test and does not affect normal operation.

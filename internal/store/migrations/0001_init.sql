-- 0001_init: initial Skald schema.
-- Timestamps are UTC ISO-8601 text. Schema version is owned by the Go runner.

CREATE TABLE projects (
  id          INTEGER PRIMARY KEY,
  slug        TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL,
  description TEXT,
  status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
  repo_path   TEXT UNIQUE,
  settings    TEXT NOT NULL DEFAULT '{}',
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE features (
  id          INTEGER PRIMARY KEY,
  project_id  INTEGER NOT NULL REFERENCES projects (id),
  title       TEXT NOT NULL,
  description TEXT,
  status      TEXT NOT NULL DEFAULT 'planned'
              CHECK (status IN ('planned', 'active', 'done', 'cancelled')),
  weight      INTEGER NOT NULL DEFAULT 1 CHECK (weight BETWEEN 1 AND 10),
  position    INTEGER NOT NULL DEFAULT 0,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE tasks (
  id                  INTEGER PRIMARY KEY,
  project_id          INTEGER NOT NULL REFERENCES projects (id),
  feature_id          INTEGER NOT NULL REFERENCES features (id),
  title               TEXT NOT NULL,
  description         TEXT,
  acceptance_criteria TEXT,
  status              TEXT NOT NULL DEFAULT 'todo'
                      CHECK (status IN ('todo', 'in_progress', 'blocked', 'done', 'cancelled')),
  priority            INTEGER NOT NULL DEFAULT 2 CHECK (priority BETWEEN 0 AND 3),
  weight              INTEGER NOT NULL DEFAULT 1 CHECK (weight BETWEEN 1 AND 10),
  position            INTEGER NOT NULL DEFAULT 0,
  assignee            TEXT,
  blocked_reason      TEXT,
  idempotency_key     TEXT,
  version             INTEGER NOT NULL DEFAULT 1,
  started_at          TEXT,
  completed_at        TEXT,
  created_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
);

CREATE TABLE task_dependencies (
  task_id       INTEGER NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
  depends_on_id INTEGER NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
  PRIMARY KEY (task_id, depends_on_id),
  CHECK (task_id <> depends_on_id)
);

-- Append-only change log; id is the change cursor.
CREATE TABLE events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id  INTEGER NOT NULL,
  entity_type TEXT NOT NULL
              CHECK (entity_type IN ('project', 'feature', 'task', 'artifact', 'document')),
  entity_id   INTEGER NOT NULL,
  type        TEXT NOT NULL,
  actor       TEXT NOT NULL,
  payload     TEXT NOT NULL DEFAULT '{}',
  created_at  TEXT NOT NULL
);

CREATE TRIGGER events_no_update BEFORE UPDATE ON events
BEGIN
  SELECT RAISE(ABORT, 'events is append-only');
END;

CREATE TRIGGER events_no_delete BEFORE DELETE ON events
BEGIN
  SELECT RAISE(ABORT, 'events is append-only');
END;

CREATE TABLE artifacts (
  id         INTEGER PRIMARY KEY,
  task_id    INTEGER NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
  kind       TEXT NOT NULL CHECK (kind IN ('commit', 'pr', 'test_report', 'link', 'manual')),
  ref        TEXT NOT NULL,
  summary    TEXT,
  actor      TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE documents (
  id              INTEGER PRIMARY KEY,
  project_id      INTEGER NOT NULL REFERENCES projects (id),
  kind            TEXT NOT NULL,
  title           TEXT NOT NULL,
  current_version INTEGER NOT NULL,
  UNIQUE (project_id, kind, title)
);

CREATE TABLE document_versions (
  document_id INTEGER NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
  version     INTEGER NOT NULL,
  body        TEXT NOT NULL,
  actor       TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  PRIMARY KEY (document_id, version)
);

CREATE INDEX idx_features_project ON features (project_id);
CREATE INDEX idx_tasks_project_status ON tasks (project_id, status);
CREATE INDEX idx_tasks_feature ON tasks (feature_id);
CREATE UNIQUE INDEX idx_tasks_idempotency ON tasks (project_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL;
CREATE INDEX idx_task_dependencies_depends_on ON task_dependencies (depends_on_id);
CREATE INDEX idx_events_project_id ON events (project_id, id);
CREATE INDEX idx_artifacts_task ON artifacts (task_id);

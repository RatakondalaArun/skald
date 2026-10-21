-- 0002_notes: the scratchpad. Notes are free text an agent or person keeps
-- about a project, optionally linked to a task or feature. They are
-- append-only and never feed progress.

CREATE TABLE notes (
  id         INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL REFERENCES projects (id),
  task_id    INTEGER REFERENCES tasks (id),
  feature_id INTEGER REFERENCES features (id),
  body       TEXT NOT NULL,
  actor      TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE INDEX idx_notes_project ON notes (project_id, id);
CREATE INDEX idx_notes_task ON notes (task_id) WHERE task_id IS NOT NULL;
CREATE INDEX idx_notes_feature ON notes (feature_id) WHERE feature_id IS NOT NULL;

CREATE TRIGGER notes_no_update BEFORE UPDATE ON notes
BEGIN
  SELECT RAISE(ABORT, 'notes are append-only');
END;

CREATE TRIGGER notes_no_delete BEFORE DELETE ON notes
BEGIN
  SELECT RAISE(ABORT, 'notes are append-only');
END;

-- events.entity_type is a CHECK constraint, which SQLite cannot alter, so the
-- table is rebuilt to accept 'note'. Ids are copied as they are, so the event
-- cursor (and AUTOINCREMENT's high-water mark) carries on unchanged.
CREATE TABLE events_new (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id  INTEGER NOT NULL,
  entity_type TEXT NOT NULL
              CHECK (entity_type IN ('project', 'feature', 'task', 'artifact', 'document', 'note')),
  entity_id   INTEGER NOT NULL,
  type        TEXT NOT NULL,
  actor       TEXT NOT NULL,
  payload     TEXT NOT NULL DEFAULT '{}',
  created_at  TEXT NOT NULL
);

INSERT INTO events_new (id, project_id, entity_type, entity_id, type, actor, payload, created_at)
  SELECT id, project_id, entity_type, entity_id, type, actor, payload, created_at FROM events ORDER BY id;

DROP TABLE events;
ALTER TABLE events_new RENAME TO events;

CREATE TRIGGER events_no_update BEFORE UPDATE ON events
BEGIN
  SELECT RAISE(ABORT, 'events is append-only');
END;

CREATE TRIGGER events_no_delete BEFORE DELETE ON events
BEGIN
  SELECT RAISE(ABORT, 'events is append-only');
END;

CREATE INDEX idx_events_project_id ON events (project_id, id);

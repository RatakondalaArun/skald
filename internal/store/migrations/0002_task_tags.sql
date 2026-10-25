-- 0002_task_tags: free-form labels on tasks.
-- tags is a JSON array of normalized, sorted, unique strings (see
-- domain.NormalizeTags). Changes are recorded as task.updated events.

ALTER TABLE tasks ADD COLUMN tags TEXT NOT NULL DEFAULT '[]';

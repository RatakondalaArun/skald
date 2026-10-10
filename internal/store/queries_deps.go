package store

import (
	"context"
	"database/sql"

	"skald/internal/domain"
)

// ListDependencyIDs returns the IDs of the tasks taskID directly depends on.
func ListDependencyIDs(ctx context.Context, q Querier, taskID int64) ([]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT depends_on_id FROM task_dependencies WHERE task_id = ? ORDER BY depends_on_id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListDependencyTasks returns the tasks taskID directly depends on.
func ListDependencyTasks(ctx context.Context, q Querier, taskID int64) ([]domain.Task, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+prefixCols("t.", taskCols)+` FROM tasks t JOIN task_dependencies d ON d.depends_on_id = t.id WHERE d.task_id = ? ORDER BY t.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListDependents returns the IDs of the tasks that directly depend on taskID.
func ListDependents(ctx context.Context, q Querier, taskID int64) ([]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT task_id FROM task_dependencies WHERE depends_on_id = ? ORDER BY task_id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListProjectDependencies returns every dependency edge between tasks of a
// project.
func ListProjectDependencies(ctx context.Context, q Querier, projectID int64) ([]domain.Dependency, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT d.task_id, d.depends_on_id FROM task_dependencies d JOIN tasks t ON t.id = d.task_id
		 WHERE t.project_id = ? ORDER BY d.task_id, d.depends_on_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Dependency
	for rows.Next() {
		var d domain.Dependency
		if err := rows.Scan(&d.TaskID, &d.DependsOnID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// FindDependencyPath looks for a chain of dependencies leading from task from
// to task to (from depends on x, x depends on y, ... on to). It returns the
// IDs along the chain including both ends, or nil when to is not reachable.
// Run it inside the write transaction that will add the edge.
func FindDependencyPath(ctx context.Context, q Querier, from, to int64) ([]int64, error) {
	if from == to {
		return []int64{from}, nil
	}
	// Breadth-first search so the reported cycle is the shortest one.
	prev := map[int64]int64{from: 0}
	queue := []int64{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		deps, err := ListDependencyIDs(ctx, q, cur)
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			if _, seen := prev[d]; seen {
				continue
			}
			prev[d] = cur
			if d == to {
				var path []int64
				for n := to; n != 0; n = prev[n] {
					path = append([]int64{n}, path...)
				}
				return path, nil
			}
			queue = append(queue, d)
		}
	}
	return nil, nil
}

// AddDependency records that taskID depends on dependsOnID and appends
// task.dependency_added. Both tasks must exist in the same project. The
// cycle check runs in the same transaction; a cycle returns a
// DependencyCycleError whose Path starts and ends at taskID. Adding an edge
// that already exists is a no-op (added == false, no event).
func AddDependency(ctx context.Context, tx *sql.Tx, m Meta, taskID, dependsOnID int64) (added bool, err error) {
	if err := m.check(); err != nil {
		return false, err
	}
	if taskID == dependsOnID {
		return false, &domain.ValidationError{Field: "dependency", Problem: domain.FormatID(domain.EntityTask, taskID) + " cannot depend on itself", Hint: "Pick a different prerequisite task."}
	}
	t, err := GetTask(ctx, tx, taskID)
	if err != nil {
		return false, err
	}
	dep, err := GetTask(ctx, tx, dependsOnID)
	if err != nil {
		return false, err
	}
	if t.ProjectID != dep.ProjectID {
		return false, &domain.ValidationError{Field: "dependency", Problem: "tasks " + domain.FormatID(domain.EntityTask, taskID) + " and " + domain.FormatID(domain.EntityTask, dependsOnID) + " are in different projects", Hint: "Dependencies must stay inside one project."}
	}
	// Adding taskID -> dependsOnID closes a cycle if dependsOnID already
	// reaches taskID.
	if path, err := FindDependencyPath(ctx, tx, dependsOnID, taskID); err != nil {
		return false, err
	} else if path != nil {
		return false, &domain.DependencyCycleError{Path: append([]int64{taskID}, path...)}
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO task_dependencies (task_id, depends_on_id) VALUES (?, ?)`, taskID, dependsOnID)
	if err != nil {
		return false, mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if err := touchTask(ctx, tx, m, taskID); err != nil {
		return false, err
	}
	_, err = appendEvent(ctx, tx, m, t.ProjectID, domain.EntityTask, taskID, domain.EventTaskDepAdded, map[string]any{"depends_on": dependsOnID})
	return true, err
}

// RemoveDependency deletes a dependency edge and appends
// task.dependency_removed. Removing an edge that does not exist is a no-op.
func RemoveDependency(ctx context.Context, tx *sql.Tx, m Meta, taskID, dependsOnID int64) (removed bool, err error) {
	if err := m.check(); err != nil {
		return false, err
	}
	t, err := GetTask(ctx, tx, taskID)
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM task_dependencies WHERE task_id = ? AND depends_on_id = ?`, taskID, dependsOnID)
	if err != nil {
		return false, mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if err := touchTask(ctx, tx, m, taskID); err != nil {
		return false, err
	}
	_, err = appendEvent(ctx, tx, m, t.ProjectID, domain.EntityTask, taskID, domain.EventTaskDepRemoved, map[string]any{"depends_on": dependsOnID})
	return true, err
}

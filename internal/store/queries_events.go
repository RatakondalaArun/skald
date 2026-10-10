package store

import (
	"context"
	"encoding/json"

	"skald/internal/domain"
)

const eventCols = `id, project_id, entity_type, entity_id, type, actor, payload, created_at`

func scanEvent(sc scanner) (domain.Event, error) {
	var (
		e                                    domain.Event
		entity, typ, actor, payload, created string
	)
	if err := sc.Scan(&e.ID, &e.ProjectID, &entity, &e.EntityID, &typ, &actor, &payload, &created); err != nil {
		return domain.Event{}, err
	}
	e.EntityType, e.Type, e.Actor, e.Payload = domain.EntityType(entity), domain.EventType(typ), domain.Actor(actor), json.RawMessage(payload)
	var err error
	e.CreatedAt, err = parseTime(created)
	return e, err
}

func queryEvents(ctx context.Context, q Querier, query string, args ...any) ([]domain.Event, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EventsSince returns events with ID greater than cursor, oldest first. A
// projectID of 0 means every project. A limit of 0 or less means no limit.
// The ID of the last event returned is the next cursor.
func EventsSince(ctx context.Context, q Querier, projectID, cursor int64, limit int) ([]domain.Event, error) {
	query := `SELECT ` + eventCols + ` FROM events WHERE id > ?`
	args := []any{cursor}
	if projectID != 0 {
		query += ` AND project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY id`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	return queryEvents(ctx, q, query, args...)
}

// MaxEventID returns the highest event ID, or 0 when there are none. A
// projectID of 0 means every project.
func MaxEventID(ctx context.Context, q Querier, projectID int64) (int64, error) {
	query := `SELECT COALESCE(MAX(id), 0) FROM events`
	var args []any
	if projectID != 0 {
		query += ` WHERE project_id = ?`
		args = append(args, projectID)
	}
	var id int64
	err := q.QueryRowContext(ctx, query, args...).Scan(&id)
	return id, err
}

// TaskEvents returns a task's history, newest first: events on the task itself
// and artifact events that name it. A limit of 0 or less means no limit.
func TaskEvents(ctx context.Context, q Querier, taskID int64, limit int) ([]domain.Event, error) {
	query := `SELECT ` + eventCols + ` FROM events
		WHERE (entity_type = 'task' AND entity_id = ?)
		   OR (entity_type = 'artifact' AND json_extract(payload, '$.task_id') = ?)
		ORDER BY id DESC`
	args := []any{taskID, taskID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	return queryEvents(ctx, q, query, args...)
}

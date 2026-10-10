package watch

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"skald/internal/domain"
	"skald/internal/store"
)

// DefaultInterval is how often the database is checked for changes.
const DefaultInterval = 300 * time.Millisecond

const (
	// batchSize is how many events one query fetches.
	batchSize = 500
	// safetyEvery forces a drain even when data_version has not moved. It is a
	// belt for the rare case where a change signal is missed; it costs one
	// indexed MAX(id)-style query every couple of seconds.
	safetyEvery = 2 * time.Second
	minBackoff  = 100 * time.Millisecond
	maxBackoff  = 5 * time.Second
	chanBuffer  = 64
)

type config struct{ interval time.Duration }

// Option configures Subscribe.
type Option func(*config)

// WithInterval sets the polling interval.
func WithInterval(d time.Duration) Option { return func(c *config) { c.interval = d } }

// Subscribe streams events with id > afterID in strictly increasing id order,
// with no gaps or duplicates. projectID 0 means all projects. The channel is
// closed when ctx is cancelled. It reads only committed rows, so it also sees
// events written by other processes.
//
// How changes are detected: one dedicated read connection runs
// PRAGMA data_version every interval. SQLite changes that value whenever any
// OTHER connection commits, which includes the writer pool of this same
// process, so writes from the CLI, from MCP servers and from this process are
// all seen. data_version is read BEFORE the events are queried, so a commit
// that lands while draining moves the value again and triggers another drain on
// the next tick. A drain also happens once at start and at least every
// safetyEvery regardless of data_version.
//
// Why there are no gaps: events.id is assigned inside write transactions, and
// writes are serialised (BEGIN IMMEDIATE, one writer at a time), so a higher id
// is never committed before a lower one. Reading committed rows in id order
// from a cursor therefore cannot skip an event.
//
// A slow consumer applies backpressure (the watcher blocks on send) but
// cancelling ctx always ends the stream. Database errors are logged to stderr
// and retried with backoff; the cursor is kept, so nothing is lost or repeated.
func Subscribe(ctx context.Context, db *store.DB, projectID, afterID int64, opts ...Option) <-chan domain.Event {
	cfg := config{interval: DefaultInterval}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.interval <= 0 {
		cfg.interval = DefaultInterval
	}
	ch := make(chan domain.Event, chanBuffer)
	w := &watcher{db: db, projectID: projectID, cursor: afterID, interval: cfg.interval, out: ch}
	go func() {
		defer close(ch)
		w.run(ctx)
	}()
	return ch
}

type watcher struct {
	db        *store.DB
	projectID int64
	cursor    int64
	interval  time.Duration
	out       chan<- domain.Event
}

func (w *watcher) run(ctx context.Context) {
	var (
		conn      *sql.Conn
		lastVer   int64 = -1
		lastDrain time.Time
		backoff   = minBackoff
	)
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()

	timer := time.NewTimer(0) // first check happens immediately
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		err := func() error {
			if conn == nil {
				c, err := w.db.Reader().Conn(ctx)
				if err != nil {
					return err
				}
				conn = c
				lastVer = -1 // force a drain on a fresh connection
			}
			var ver int64
			if err := conn.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&ver); err != nil {
				return err
			}
			if ver == lastVer && time.Since(lastDrain) < safetyEvery {
				return nil
			}
			if err := w.drain(ctx, conn); err != nil {
				return err
			}
			lastVer, lastDrain = ver, time.Now()
			return nil
		}()

		next := w.interval
		switch {
		case err == nil:
			backoff = minBackoff
		case ctx.Err() != nil:
			return
		default:
			slog.Warn("watch: database read failed, retrying", "error", err, "retry_in", backoff)
			if conn != nil {
				conn.Close()
				conn = nil
			}
			next = backoff
			backoff = min(backoff*2, maxBackoff)
		}
		timer.Reset(next)
	}
}

// drain emits every event after the cursor, advancing the cursor only after an
// event has been delivered.
func (w *watcher) drain(ctx context.Context, q store.Querier) error {
	for {
		events, err := store.EventsSince(ctx, q, w.projectID, w.cursor, batchSize)
		if err != nil {
			return err
		}
		for _, ev := range events {
			select {
			case w.out <- ev:
				w.cursor = ev.ID
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if len(events) < batchSize {
			return nil
		}
	}
}

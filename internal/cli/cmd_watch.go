package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"skald/internal/domain"
	"skald/internal/watch"
)

const (
	// redrawEvery is the shortest time between two redraws of `status --watch`.
	redrawEvery = 250 * time.Millisecond
	// refreshEvery redraws even without events, so time-based facts (stale
	// tasks) stay current.
	refreshEvery = 30 * time.Second
)

// signalContext is cancelled by Ctrl-C or SIGTERM, which ends a follow or watch
// command normally (exit 0).
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

// followLog prints the backlog and then streams new events until interrupted.
// pid 0 means every project.
func followLog(e *env, pid, since int64, sinceSet bool, limit int) error {
	ctx, stop := signalContext(e.ctx)
	defer stop()

	var cursor int64
	if sinceSet {
		cursor = since // Subscribe replays everything after it
	} else {
		backlog, err := recentEvents(e, pid, limit)
		if err != nil {
			return err
		}
		for _, ev := range backlog {
			if err := printEventLine(e, ev); err != nil {
				return err
			}
			cursor = ev.ID
		}
	}
	// With no backlog the project has no events yet, so starting from 0 only
	// delivers events written from now on.
	for ev := range watch.Subscribe(ctx, e.db, pid, cursor) {
		if err := printEventLine(e, ev); err != nil {
			return err
		}
	}
	return nil
}

// printEventLine writes one event: a JSON object per line with --json, else a
// single text line.
func printEventLine(e *env, ev domain.Event) error {
	if e.json {
		b, err := json.Marshal(toEvents([]domain.Event{ev})[0])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(e.out, "%s\n", b)
		return err
	}
	_, err := fmt.Fprintf(e.out, "%d  %s  %s  %s  %s  %s\n", ev.ID, ev.CreatedAt.UTC().Format("2006-01-02 15:04:05"), ev.Actor,
		ev.Type, domain.FormatID(ev.EntityType, ev.EntityID), eventDetail(ev))
	return err
}

// watchStatus redraws the status view whenever the project changes.
func watchStatus(e *env) error {
	ctx, stop := signalContext(e.ctx)
	defer stop()
	e.ctx = ctx

	rep, ready, err := loadStatus(e, e.ref)
	if err != nil {
		return err
	}
	pid := rep.Project.ID
	// Subscribe after the first read: anything written in between shows up as
	// an event and triggers a redraw, so no change is missed.
	latest, err := e.svc.LatestEventID(ctx, pid)
	if err != nil {
		return err
	}
	events := watch.Subscribe(ctx, e.db, pid, latest)

	tty := isTerminal(e.out)
	first := true
	render := func() {
		var buf bytes.Buffer
		writeStatus(&buf, rep, ready)
		if tty {
			drawScreen(e.out, buf.String(), "watching "+rep.Project.Slug+" - Ctrl-C to exit")
			return
		}
		if !first {
			fmt.Fprintln(e.out)
		}
		e.out.Write(buf.Bytes())
	}
	if tty {
		fmt.Fprint(e.out, "\x1b[?25l") // hide the cursor
		defer fmt.Fprint(e.out, "\x1b[?25h\n")
	}
	render()
	first = false

	tick := time.NewTicker(redrawEvery)
	defer tick.Stop()
	slow := time.NewTicker(refreshEvery)
	defer slow.Stop()
	dirty := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-events:
			if !ok {
				return nil
			}
			dirty = true
		case <-slow.C:
			dirty = true
		case <-tick.C:
			if !dirty {
				continue
			}
			dirty = false
			r, rd, err := loadStatus(e, e.ref)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				fmt.Fprintf(e.errw, "skald: refresh failed: %v\n", err)
				continue
			}
			rep, ready = r, rd
			render()
		}
	}
}

// drawScreen repaints the terminal in place: home the cursor, rewrite each line
// clearing its tail, then clear whatever is left below. That avoids the flicker
// of clearing the whole screen first.
func drawScreen(w io.Writer, body, footer string) {
	body = strings.TrimRight(body, "\n")
	var b strings.Builder
	b.WriteString("\x1b[H")
	b.WriteString(strings.ReplaceAll(body, "\n", "\x1b[K\n"))
	b.WriteString("\x1b[K\n\n\x1b[2m")
	b.WriteString(footer)
	b.WriteString("\x1b[0m\x1b[K\n\x1b[J")
	io.WriteString(w, b.String())
}

// isTerminal reports whether w is a character device (a terminal). It avoids a
// dependency on a terminal package.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"skald/internal/domain"
	"skald/internal/service"
)

// enricher turns domain events into API events: it adds the project slug and,
// for events about a task, the task id and title, so the feed can say "started
// T-12 Login endpoint" without the client looking anything up. Lookups are
// cached; a failed lookup just leaves the title out.
type enricher struct {
	svc      *service.Service
	mu       sync.Mutex
	projects map[int64]string // project id -> slug
	titles   map[int64]string // task id -> title
}

const maxCachedTitles = 5000

func newEnricher(svc *service.Service) *enricher {
	return &enricher{svc: svc, projects: map[int64]string{}, titles: map[int64]string{}}
}

func (e *enricher) slug(ctx context.Context, projectID int64) string {
	e.mu.Lock()
	s, ok := e.projects[projectID]
	e.mu.Unlock()
	if ok {
		return s
	}
	list, err := e.svc.ListProjects(ctx, webActor, true)
	if err != nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, o := range list {
		e.projects[o.Project.ID] = o.Project.Slug
	}
	return e.projects[projectID]
}

func (e *enricher) title(ctx context.Context, slug string, taskID int64) string {
	e.mu.Lock()
	t, ok := e.titles[taskID]
	e.mu.Unlock()
	if ok || slug == "" {
		return t
	}
	d, err := e.svc.GetTask(ctx, webActor, service.ProjectRef{Slug: slug}, taskID)
	if err != nil {
		return ""
	}
	e.setTitle(taskID, d.Task.Title)
	return d.Task.Title
}

func (e *enricher) setTitle(taskID int64, title string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.titles) >= maxCachedTitles {
		e.titles = map[int64]string{}
	}
	e.titles[taskID] = title
}

func (e *enricher) event(ctx context.Context, ev domain.Event) eventDTO {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	payload := ev.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	d := eventDTO{
		ID: ev.ID, Project: e.slug(ctx, ev.ProjectID), EntityType: string(ev.EntityType),
		Entity: domain.FormatID(ev.EntityType, ev.EntityID), Type: string(ev.Type),
		Actor: string(ev.Actor), Payload: payload, At: ts(ev.CreatedAt),
	}
	var taskID int64
	switch ev.EntityType {
	case domain.EntityTask:
		taskID = ev.EntityID
	case domain.EntityArtifact, domain.EntityNote:
		var p struct {
			TaskID int64 `json:"task_id"`
		}
		_ = json.Unmarshal(payload, &p)
		taskID = p.TaskID
	}
	if taskID == 0 {
		return d
	}
	d.Task = domain.FormatID(domain.EntityTask, taskID)
	switch {
	case ev.Type == domain.EventTaskCreated:
		var p struct {
			Title string `json:"title"`
		}
		if json.Unmarshal(payload, &p) == nil && p.Title != "" {
			e.setTitle(taskID, p.Title)
			d.Title = p.Title
			return d
		}
	case ev.Type == domain.EventTaskUpdated:
		e.mu.Lock()
		delete(e.titles, taskID) // the title may have changed
		e.mu.Unlock()
	}
	d.Title = e.title(ctx, d.Project, taskID)
	return d
}

// serveSSE streams the change feed. Event ids are skald event ids, so a client
// that reconnects with Last-Event-ID gets exactly the events it missed: the
// EventSource replays them from the durable log starting after that id.
// Without Last-Event-ID the stream starts at the newest event, and the page
// refreshes its data when the stream opens, which covers the gap.
func (s *Server) serveSSE(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rc := http.NewResponseController(w)
	var projectID int64
	if slug := r.URL.Query().Get("project"); slug != "" {
		p, err := s.cfg.Service.ResolveProject(ctx, service.ProjectRef{Slug: slug})
		if err != nil {
			s.fail(w, r, err)
			return
		}
		projectID = p.ID
	}
	after, have, err := lastEventID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !have {
		if after, err = s.cfg.Service.LatestEventID(ctx, projectID); err != nil {
			s.fail(w, r, err)
			return
		}
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(format string, a ...any) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if _, err := fmt.Fprintf(w, format, a...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !write("retry: 2000\n: connected\n\n") {
		return
	}

	ch := s.cfg.Events(ctx, projectID, after)
	tick := time.NewTicker(s.cfg.Keepalive)
	defer tick.Stop()
	last := after
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.ID <= last {
				continue // never send an id twice or out of order
			}
			data, err := json.Marshal(s.enrich.event(ctx, ev))
			if err != nil {
				s.log.Error("encode event", "id", ev.ID, "err", err)
				continue
			}
			if !write("id: %d\ndata: %s\n\n", ev.ID, data) {
				return
			}
			last = ev.ID
		case <-tick.C:
			if !write(": keepalive\n\n") {
				return
			}
		}
	}
}

// lastEventID reads the resume point from the Last-Event-ID header or the
// ?after= parameter.
func lastEventID(r *http.Request) (id int64, have bool, err error) {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("after")
	}
	if v == "" {
		return 0, false, nil
	}
	id, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil || id < 0 {
		return 0, false, &domain.ValidationError{Field: "Last-Event-ID", Problem: "must be an event id of 0 or more"}
	}
	return id, true, nil
}

package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"skald/internal/domain"
	"skald/internal/service"
)

// webActor is who the dashboard acts as. It counts as a human (it cannot
// force completion) and shows up separately from the CLI in the activity feed.
const webActor domain.Actor = "human:web"

const (
	maxBoardTasks = 2000
	boardPage     = service.MaxListLimit
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, kind, msg string) {
	var b errorBody
	b.Error.Kind, b.Error.Message = kind, msg
	writeJSON(w, status, b)
}

// fail maps an error to a response. Domain errors carry a message that says
// what to do next and are passed through; anything else is logged and replaced
// by a generic message so internals do not leak.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if status, b, ok := s.errorResponse(r, err); ok {
		writeJSON(w, status, b)
	}
}

// errorResponse is the status and body fail sends for err; ok is false when
// the client went away and nothing should be sent.
func (s *Server) errorResponse(r *http.Request, err error) (status int, b errorBody, ok bool) {
	if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
		return 0, b, false // the client went away
	}
	var de domain.Error
	if !errors.As(err, &de) {
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		b.Error.Kind, b.Error.Message = "internal", "Something went wrong inside skald. The details are in the skald serve log on stderr."
		return http.StatusInternalServerError, b, true
	}
	status = http.StatusInternalServerError
	switch de.Kind() {
	case domain.KindNotFound:
		status = http.StatusNotFound
	case domain.KindValidation:
		status = http.StatusBadRequest
	case domain.KindConflict:
		status = http.StatusConflict
	case domain.KindInvalidTransition, domain.KindEvidenceRequired, domain.KindDependencyCycle:
		status = http.StatusUnprocessableEntity
	}
	b.Error.Kind, b.Error.Message = de.Kind().String(), de.Message()
	var ce *domain.ConflictError
	if errors.As(err, &ce) && ce.CurrentTask != nil {
		t := taskOf(*ce.CurrentTask)
		b.Error.CurrentTask = &t
	}
	if status == http.StatusInternalServerError {
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		b.Error.Message = "Something went wrong inside skald. The details are in the skald serve log on stderr."
	}
	return status, b, true
}

func ref(r *http.Request) service.ProjectRef { return service.ProjectRef{Slug: r.PathValue("slug")} }

func intParam(r *http.Request, name string, def, min, max int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		return 0, &domain.ValidationError{Field: name, Problem: "must be a whole number from " + strconv.Itoa(min) + " to " + strconv.Itoa(max)}
	}
	return n, nil
}

func taskID(r *http.Request) (int64, error) {
	return domain.ParseID(domain.EntityTask, r.PathValue("id"))
}

// ---- reads ----------------------------------------------------------------

func (s *Server) apiProjects(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Service.ListProjects(r.Context(), webActor, r.URL.Query().Get("archived") == "1")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]overviewDTO, 0, len(list))
	for _, o := range list {
		out = append(out, overviewDTO{
			Project: projectOf(o.Project), Progress: progressOf(o.Progress.Progress),
			Counts: statusCountsOf(o.Counts), Features: o.Features, Ready: o.Ready,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

func (s *Server) apiProject(w http.ResponseWriter, r *http.Request) {
	sum, err := s.cfg.Service.ProjectSummary(r.Context(), ref(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	links := service.NewGitHubLinks(sum.Project)
	features := make([]featureDTO, 0, len(sum.Features))
	for _, f := range sum.Features {
		d := featureDTO{
			ID: domain.FormatID(domain.EntityFeature, f.Feature.ID), Title: f.Feature.Title,
			Description: f.Feature.Description, Status: string(f.Status), Weight: f.Feature.Weight,
			Position: f.Feature.Position, Progress: progressOf(f.Progress), Counts: countsOf(f.Counts), Counted: f.Counted,
		}
		d.linkGitHub(links, f.Feature.ID)
		features = append(features, d)
	}
	type blockedDTO struct {
		Task   taskDTO `json:"task"`
		Reason string  `json:"reason"`
	}
	blocked := make([]blockedDTO, 0, len(sum.Blocked))
	for _, b := range sum.Blocked {
		blocked = append(blocked, blockedDTO{taskIn(links, b.Task), b.Reason})
	}
	docs := make([]documentDTO, 0, len(sum.Documents))
	for _, d := range sum.Documents {
		docs = append(docs, documentOf(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project":     projectOf(sum.Project),
		"progress":    progressOf(sum.Progress),
		"counts":      countsOf(sum.Counts),
		"ready_count": sum.ReadyCount,
		"features":    features,
		"blocked":     blocked,
		"stale":       tasksIn(links, sum.Stale),
		"documents":   docs,
	})
}

func (s *Server) apiFeatures(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Service.ListFeatures(r.Context(), webActor, ref(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	links := s.githubLinks(r)
	out := make([]featureDTO, 0, len(list))
	for _, f := range list {
		d := featureDTO{
			ID: domain.FormatID(domain.EntityFeature, f.Feature.ID), Title: f.Feature.Title,
			Description: f.Feature.Description, Status: string(f.Feature.Status), Weight: f.Feature.Weight,
			Position: f.Feature.Position, Progress: progressOf(f.Progress), Counts: statusCountsOf(f.Counts), Counted: true,
		}
		d.linkGitHub(links, f.Feature.ID)
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"features": out})
}

// githubLinks reads the GitHub links of the request's project. A project that
// cannot be read links nothing; the handler reports its own error.
func (s *Server) githubLinks(r *http.Request) service.GitHubLinks {
	p, err := s.cfg.Service.ResolveProject(r.Context(), ref(r))
	if err != nil {
		return service.GitHubLinks{}
	}
	return service.NewGitHubLinks(p)
}

// allTasks pages through ListTasks. The second result says the cap was hit.
func (s *Server) allTasks(ctx context.Context, ref service.ProjectRef, f service.ListTasksFilter) ([]domain.Task, bool, error) {
	var all []domain.Task
	for off := 0; ; off += boardPage {
		f.Limit, f.Offset = boardPage, off
		page, err := s.cfg.Service.ListTasks(ctx, webActor, ref, f)
		if err != nil {
			return nil, false, err
		}
		all = append(all, page.Tasks...)
		if !page.Truncated {
			return all, false, nil
		}
		if len(all) >= maxBoardTasks {
			return all, true, nil
		}
	}
}

// apiBoard returns every task of a project (priority order), with ready flags
// and the stale set, for the board and the progress strip.
func (s *Server) apiBoard(w http.ResponseWriter, r *http.Request) {
	rf := ref(r)
	tasks, capped, err := s.allTasks(r.Context(), rf, service.ListTasksFilter{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ready, _, err := s.allTasks(r.Context(), rf, service.ListTasksFilter{ReadyOnly: true})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	prog, err := s.cfg.Service.ProjectProgress(r.Context(), rf)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	isReady := make(map[int64]bool, len(ready))
	for _, t := range ready {
		isReady[t.ID] = true
	}
	isStale := make(map[int64]bool, len(prog.Stale))
	for _, t := range prog.Stale {
		isStale[t.ID] = true
	}
	links := service.NewGitHubLinks(prog.Project)
	out := make([]taskDTO, 0, len(tasks))
	for _, t := range tasks {
		d := taskIn(links, t)
		d.Description, d.AcceptanceCriteria = "", "" // the board does not need the long text
		d.Ready, d.Stale = isReady[t.ID], isStale[t.ID]
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": prog.Project.Slug, "tasks": out, "capped": capped})
}

func (s *Server) apiTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := service.ListTasksFilter{ReadyOnly: q.Get("ready") == "1"}
	var err error
	if f.Limit, err = intParam(r, "limit", 0, 0, service.MaxListLimit); err != nil {
		s.fail(w, r, err)
		return
	}
	if f.Offset, err = intParam(r, "offset", 0, 0, 1_000_000); err != nil {
		s.fail(w, r, err)
		return
	}
	if v := q.Get("feature"); v != "" {
		if f.FeatureID, err = domain.ParseID(domain.EntityFeature, v); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	for _, st := range strings.Split(q.Get("status"), ",") {
		if st = strings.TrimSpace(st); st == "" {
			continue
		}
		ts := domain.TaskStatus(st)
		if !ts.Valid() {
			s.fail(w, r, &domain.ValidationError{Field: "status", Problem: "unknown status " + strconv.Quote(st) + "; use todo, in_progress, blocked, done or cancelled"})
			return
		}
		f.Statuses = append(f.Statuses, ts)
	}
	for _, tag := range q["tag"] {
		for _, t := range strings.Split(tag, ",") {
			if t = strings.TrimSpace(t); t != "" {
				f.Tags = append(f.Tags, t)
			}
		}
	}
	list, err := s.cfg.Service.ListTasks(r.Context(), webActor, ref(r), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasksIn(s.githubLinks(r), list.Tasks), "truncated": list.Truncated})
}

func (s *Server) apiTask(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d, err := s.cfg.Service.GetTask(r.Context(), webActor, ref(r), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type linkDTO struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	link := func(ts []domain.Task) []linkDTO {
		out := make([]linkDTO, 0, len(ts))
		for _, t := range ts {
			out = append(out, linkDTO{domain.FormatID(domain.EntityTask, t.ID), t.Title, string(t.Status)})
		}
		return out
	}
	arts := make([]artifactDTO, 0, len(d.Artifacts))
	for _, a := range d.Artifacts {
		arts = append(arts, artifactDTO{domain.FormatID(domain.EntityArtifact, a.ID), string(a.Kind), a.Ref, a.Summary, string(a.Actor), ts(a.CreatedAt)})
	}
	slug := r.PathValue("slug")
	events := make([]eventDTO, 0, len(d.RecentEvents))
	for _, e := range d.RecentEvents {
		events = append(events, s.enrich.event(r.Context(), e))
	}
	unmet := make([]string, 0, len(d.UnmetDependencies))
	for _, u := range d.UnmetDependencies {
		unmet = append(unmet, domain.FormatID(domain.EntityTask, u))
	}
	task := taskIn(s.githubLinks(r), d.Task)
	task.Ready, task.Stale = d.Ready, d.Stale
	writeJSON(w, http.StatusOK, map[string]any{
		"project":      slug,
		"task":         task,
		"feature":      map[string]string{"id": domain.FormatID(domain.EntityFeature, d.Feature.ID), "title": d.Feature.Title},
		"dependencies": link(d.Dependencies),
		"dependents":   link(d.Dependents),
		"unmet":        unmet,
		"artifacts":    arts,
		"events":       events,
	})
}

func (s *Server) apiNext(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit", service.DefaultNextLimit, 1, service.MaxNextLimit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.cfg.Service.NextTasks(r.Context(), ref(r), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasksIn(s.githubLinks(r), res.Tasks), "total": res.Total, "truncated": res.Truncated})
}

func (s *Server) apiDocuments(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Service.ListDocuments(r.Context(), ref(r), r.URL.Query().Get("kind"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]documentDTO, 0, len(list))
	for _, d := range list {
		out = append(out, documentOf(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": out})
}

func (s *Server) apiDocument(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	version, err := intParam(r, "version", 0, 0, 1_000_000)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d, v, err := s.cfg.Service.GetDocument(r.Context(), ref(r), q.Get("kind"), q.Get("title"), version)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"document": documentOf(d), "version": v.Version, "body": v.Body,
		"actor": string(v.Actor), "created_at": ts(v.CreatedAt),
	})
}

// apiProjectEvents and apiEvents serve the change feed. With ?cursor= they
// page forward from that event id; without it they return the most recent
// events (oldest first within the page).
func (s *Server) apiProjectEvents(w http.ResponseWriter, r *http.Request) {
	s.events(w, r, r.PathValue("slug"))
}
func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	s.events(w, r, r.URL.Query().Get("project"))
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, slug string) {
	ctx := r.Context()
	var projectID int64
	if slug != "" {
		p, err := s.cfg.Service.ResolveProject(ctx, service.ProjectRef{Slug: slug})
		if err != nil {
			s.fail(w, r, err)
			return
		}
		projectID = p.ID
	}
	limit, err := intParam(r, "limit", 50, 1, service.MaxEventLimit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var (
		evs    []domain.Event
		cursor int64
		more   bool
	)
	if v := r.URL.Query().Get("cursor"); v != "" {
		c, err := strconv.ParseInt(v, 10, 64)
		if err != nil || c < 0 {
			s.fail(w, r, &domain.ValidationError{Field: "cursor", Problem: "must be an event id of 0 or more"})
			return
		}
		page, err := s.cfg.Service.EventsSince(ctx, projectID, c, limit)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		evs, cursor, more = page.Events, page.Cursor, page.More
	} else if evs, cursor, err = s.recentEvents(ctx, projectID, limit); err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]eventDTO, 0, len(evs))
	for _, e := range evs {
		out = append(out, s.enrich.event(ctx, e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out, "cursor": cursor, "more": more})
}

// recentEvents returns the last n events of a project. EventsSince only pages
// forward, so it starts a window back from the newest id and widens it until
// enough events are found or the log's start is reached.
func (s *Server) recentEvents(ctx context.Context, projectID int64, n int) ([]domain.Event, int64, error) {
	latest, err := s.cfg.Service.LatestEventID(ctx, projectID)
	if err != nil {
		return nil, 0, err
	}
	window := int64(n) * 4
	for {
		start := latest - window
		if start < 0 {
			start = 0
		}
		var got []domain.Event
		cur := start
		for {
			page, err := s.cfg.Service.EventsSince(ctx, projectID, cur, service.MaxEventLimit)
			if err != nil {
				return nil, 0, err
			}
			got = append(got, page.Events...)
			cur = page.Cursor
			if !page.More {
				break
			}
		}
		if len(got) >= n || start == 0 {
			if len(got) > n {
				got = got[len(got)-n:]
			}
			return got, latest, nil
		}
		window *= 4
	}
}

// ---- writes ---------------------------------------------------------------

type actionBody struct {
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason"` // block
	Note            string `json:"note"`   // done
	Force           bool   `json:"force"`  // done: override the evidence gate (humans only)
}

// apiTaskAction handles the status changes the dashboard may make: start,
// block, unblock, done, cancel and reopen. Notes and evidence have their own
// endpoints. All rules live in the service layer, the same as for the CLI.
func (s *Server) apiTaskAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := taskID(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		var body actionBody
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			s.fail(w, r, &domain.ValidationError{Field: "body", Problem: "must be JSON like {\"expected_version\": 3}"})
			return
		}
		if body.ExpectedVersion < 0 {
			s.fail(w, r, &domain.ValidationError{Field: "expected_version", Problem: "must be 0 or more"})
			return
		}
		// Reading the task first confirms it belongs to this project and, for
		// unblock, tells whether it was ever started.
		d, err := s.cfg.Service.GetTask(r.Context(), webActor, ref(r), id)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		var to domain.TaskStatus
		switch action {
		case "unblock":
			to = domain.TaskTodo
			if d.Task.StartedAt != nil {
				to = domain.TaskInProgress
			}
		case "cancel":
			to = domain.TaskCancelled
		case "start":
			to = domain.TaskInProgress
		case "block":
			to = domain.TaskBlocked
			if strings.TrimSpace(body.Reason) == "" {
				s.fail(w, r, &domain.ValidationError{Field: "reason", Problem: "is required to block a task", Hint: "Say what the task is waiting for."})
				return
			}
		case "done":
			to = domain.TaskDone
		default:
			to = domain.TaskTodo
		}
		t, err := s.cfg.Service.UpdateTaskStatus(r.Context(), webActor, id, service.StatusChange{
			To: to, ExpectedVersion: body.ExpectedVersion,
			BlockedReason: strings.TrimSpace(body.Reason), Note: strings.TrimSpace(body.Note), Force: body.Force,
		})
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"task": taskOf(t)})
	}
}

// decodeBody reads a small JSON body into v, rejecting unknown fields.
func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, v any, example string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		s.fail(w, r, &domain.ValidationError{Field: "body", Problem: "must be JSON like " + example})
		return false
	}
	return true
}

// apiTaskNote adds a progress note to a task.
func (s *Server) apiTaskNote(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !s.decodeBody(w, r, &body, `{"text": "schema drafted"}`) {
		return
	}
	if _, err := s.cfg.Service.GetTask(r.Context(), webActor, ref(r), id); err != nil {
		s.fail(w, r, err)
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		s.fail(w, r, &domain.ValidationError{Field: "text", Problem: "is required", Hint: "Write the note first."})
		return
	}
	ev, err := s.cfg.Service.AddTaskNote(r.Context(), webActor, id, strings.TrimSpace(body.Text))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": ev})
}

// apiTaskEvidence attaches verification evidence to a task.
func (s *Server) apiTaskEvidence(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body struct {
		Kind    string `json:"kind"`
		Ref     string `json:"ref"`
		Summary string `json:"summary"`
	}
	if !s.decodeBody(w, r, &body, `{"kind": "commit", "ref": "3f2a9c1", "summary": "login endpoint"}`) {
		return
	}
	if _, err := s.cfg.Service.GetTask(r.Context(), webActor, ref(r), id); err != nil {
		s.fail(w, r, err)
		return
	}
	k := domain.ArtifactKind(body.Kind)
	if !k.Valid() {
		s.fail(w, r, &domain.ValidationError{Field: "kind", Problem: "must be one of commit, pr, test_report, link, manual"})
		return
	}
	if strings.TrimSpace(body.Ref) == "" {
		s.fail(w, r, &domain.ValidationError{Field: "ref", Problem: "is required", Hint: "Give a commit SHA, PR URL, report path or link."})
		return
	}
	art, err := s.cfg.Service.RecordArtifact(r.Context(), webActor, id, service.ArtifactInput{Kind: k, Ref: strings.TrimSpace(body.Ref), Summary: strings.TrimSpace(body.Summary)})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifact": art.ID})
}

// apiTaskEdit changes a task's title, description, criteria, priority, weight
// or tags. Only the fields present in the body change; tags replaces the set.
func (s *Server) apiTaskEdit(w http.ResponseWriter, r *http.Request) {
	id, err := taskID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body struct {
		ExpectedVersion    int64     `json:"expected_version"`
		Title              *string   `json:"title"`
		Description        *string   `json:"description"`
		AcceptanceCriteria *string   `json:"acceptance_criteria"`
		Priority           *int      `json:"priority"`
		Weight             *int      `json:"weight"`
		Tags               *[]string `json:"tags"`
	}
	if !s.decodeBody(w, r, &body, `{"expected_version": 3, "title": "New title", "priority": 1, "tags": ["ui"]}`) {
		return
	}
	if body.ExpectedVersion < 0 {
		s.fail(w, r, &domain.ValidationError{Field: "expected_version", Problem: "must be 0 or more"})
		return
	}
	if _, err := s.cfg.Service.GetTask(r.Context(), webActor, ref(r), id); err != nil {
		s.fail(w, r, err)
		return
	}
	trim := func(v *string) *string {
		if v == nil {
			return nil
		}
		t := strings.TrimSpace(*v)
		return &t
	}
	t, err := s.cfg.Service.UpdateTask(r.Context(), webActor, id, service.TaskEdit{
		Title: trim(body.Title), Description: trim(body.Description), AcceptanceCriteria: trim(body.AcceptanceCriteria),
		Priority: body.Priority, Weight: body.Weight, Tags: body.Tags, ExpectedVersion: body.ExpectedVersion,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": taskOf(t)})
}

// apiTaskDependency adds or removes one "waits for" link of a task.
func (s *Server) apiTaskDependency(add bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := taskID(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		var body struct {
			DependsOn string `json:"depends_on"`
		}
		if !s.decodeBody(w, r, &body, `{"depends_on": "T-5"}`) {
			return
		}
		on, err := domain.ParseID(domain.EntityTask, body.DependsOn)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if _, err := s.cfg.Service.GetTask(r.Context(), webActor, ref(r), id); err != nil {
			s.fail(w, r, err)
			return
		}
		// The other task must be in this project too, or the dashboard could
		// be used to link across projects.
		if _, err := s.cfg.Service.GetTask(r.Context(), webActor, ref(r), on); err != nil {
			s.fail(w, r, err)
			return
		}
		var changed bool
		if add {
			changed, err = s.cfg.Service.AddDependency(r.Context(), webActor, id, on)
		} else {
			changed, err = s.cfg.Service.RemoveDependency(r.Context(), webActor, id, on)
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"changed": changed})
	}
}

// apiGitHubSync pulls the project's linked GitHub repository, and with push
// closes issues whose tasks are finished here. See service.SyncGitHub.
//
// A client that accepts application/x-ndjson gets the sync's progress as it
// runs: one {"progress": {...}} line per update, then a last line that is
// {"result": {...}} or an error body. Errors found before the sync starts
// still come back as a plain JSON error with its status.
func (s *Server) apiGitHubSync(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Push         bool `json:"push"`
		CreateIssues bool `json:"create_issues"`
		DryRun       bool `json:"dry_run"`
	}
	if !s.decodeBody(w, r, &body, `{"push": false, "create_issues": false, "dry_run": false}`) {
		return
	}
	o := service.GitHubSyncOptions{Push: body.Push, CreateIssues: body.CreateIssues, DryRun: body.DryRun}
	var ps *progressStream
	if strings.Contains(r.Header.Get("Accept"), "application/x-ndjson") {
		ps = &progressStream{w: w, rc: http.NewResponseController(w)}
		o.Progress = ps.progress
	}
	res, err := s.cfg.Service.SyncGitHub(r.Context(), webActor, ref(r), o)
	if err != nil {
		if ps != nil && ps.started {
			if _, b, ok := s.errorResponse(r, err); ok {
				ps.line(b)
			}
			return
		}
		s.fail(w, r, err)
		return
	}
	note := func(ns []service.GitHubIssueNote) []map[string]any {
		out := make([]map[string]any, 0, len(ns))
		for _, n := range ns {
			m := map[string]any{"issue": n.Issue, "note": n.Note}
			if n.TaskID != 0 {
				m["task"] = domain.FormatID(domain.EntityTask, n.TaskID)
			}
			out = append(out, m)
		}
		return out
	}
	out := map[string]any{
		"repo": res.Repo, "dry_run": res.DryRun, "issues": res.Issues,
		"features_created": res.FeaturesCreated, "tasks_created": res.TasksCreated, "tasks_updated": res.TasksUpdated,
		"evidence_added": res.EvidenceAdded, "status_changes": len(res.StatusChanges),
		"skipped": note(res.Skipped), "diverged": note(res.Diverged), "pushed": note(res.Pushed), "push_failed": note(res.PushFailed),
		"warnings":            append([]string{}, res.Warnings...),
		"milestones_pushed":   len(res.MilestonesPushed),
		"created":             note(res.Created),
		"create_failed":       note(res.CreateFailed),
		"milestones_diverged": len(res.MilestonesDiverged),
	}
	if ps != nil && ps.started {
		ps.line(map[string]any{"result": out})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// progressStream writes a sync's progress as newline-delimited JSON. The
// response starts with the first update, so an error before it can still use
// its own status. Updates within a stage are sent at most ten times a second.
type progressStream struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
	last    service.GitHubSyncProgress
	at      time.Time
}

func (p *progressStream) progress(g service.GitHubSyncProgress) {
	if p.started && g.Stage == p.last.Stage && g.Done != g.Total && time.Since(p.at) < 100*time.Millisecond {
		return
	}
	p.last, p.at = g, time.Now()
	p.line(map[string]any{"progress": map[string]any{
		"stage": g.Stage, "done": g.Done, "total": g.Total, "message": g.String(),
	}})
}

func (p *progressStream) line(v any) {
	if !p.started {
		h := p.w.Header()
		h.Set("Content-Type", "application/x-ndjson; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Accel-Buffering", "no")
		p.w.WriteHeader(http.StatusOK)
		p.started = true
	}
	// A long sync outlives the usual write deadline; each line extends it.
	_ = p.rc.SetWriteDeadline(time.Now().Add(20 * time.Second))
	if err := json.NewEncoder(p.w).Encode(v); err == nil {
		_ = p.rc.Flush()
	}
}

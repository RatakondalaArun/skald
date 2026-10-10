package domain

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorKind classifies domain errors so entry points can map them to CLI exit
// codes, HTTP statuses or MCP error results without inspecting types.
type ErrorKind int

const (
	KindUnknown ErrorKind = iota
	KindNotFound
	KindInvalidTransition
	KindConflict
	KindDependencyCycle
	KindEvidenceRequired
	KindValidation
)

func (k ErrorKind) String() string {
	switch k {
	case KindNotFound:
		return "not_found"
	case KindInvalidTransition:
		return "invalid_transition"
	case KindConflict:
		return "conflict"
	case KindDependencyCycle:
		return "dependency_cycle"
	case KindEvidenceRequired:
		return "evidence_required"
	case KindValidation:
		return "validation"
	}
	return "unknown"
}

// Error is implemented by every typed domain error. Message is the full
// human-readable text and always names what to do next; Error returns the same.
type Error interface {
	error
	Kind() ErrorKind
	Message() string
}

// KindOf returns the kind of the first domain error in err's chain, or
// KindUnknown.
func KindOf(err error) ErrorKind {
	var de Error
	if errors.As(err, &de) {
		return de.Kind()
	}
	return KindUnknown
}

// Sentinels for errors.Is(err, domain.ErrNotFound) style checks; they match
// any error of the same type regardless of its fields.
var (
	ErrNotFound          error = &NotFoundError{}
	ErrInvalidTransition error = &InvalidTransitionError{}
	ErrConflict          error = &ConflictError{}
	ErrDependencyCycle   error = &DependencyCycleError{}
	ErrEvidenceRequired  error = &EvidenceRequiredError{}
	ErrValidation        error = &ValidationError{}
)

// NotFoundError means an entity does not exist (in the current project).
type NotFoundError struct {
	Entity EntityType
	ID     int64
	Ref    string // set instead of ID for lookups by name, such as a project slug
}

func (e *NotFoundError) Kind() ErrorKind { return KindNotFound }
func (e *NotFoundError) Error() string   { return e.Message() }
func (e *NotFoundError) Is(t error) bool { _, ok := t.(*NotFoundError); return ok }
func (e *NotFoundError) Message() string {
	what := string(e.Entity)
	if what == "" {
		what = "item"
	}
	switch {
	case e.Ref != "":
		return fmt.Sprintf("%s %q not found. List the %ss to see valid names, or check that you are in the right project.", what, e.Ref, what)
	case e.ID != 0:
		return fmt.Sprintf("%s not found. List the %ss to see valid IDs; it may also belong to a different project.", FormatID(e.Entity, e.ID), what)
	}
	return fmt.Sprintf("%s not found. List the %ss to see what exists.", what, what)
}

// InvalidTransitionError means a task status change is not allowed.
type InvalidTransitionError struct {
	TaskID    int64
	From, To  TaskStatus
	UnmetDeps []int64 // set when starting is blocked by unfinished dependencies
	Reason    string  // optional extra detail
}

func (e *InvalidTransitionError) Kind() ErrorKind { return KindInvalidTransition }
func (e *InvalidTransitionError) Error() string   { return e.Message() }
func (e *InvalidTransitionError) Is(t error) bool {
	_, ok := t.(*InvalidTransitionError)
	return ok
}
func (e *InvalidTransitionError) Message() string {
	task := "task"
	if e.TaskID != 0 {
		task = FormatID(EntityTask, e.TaskID)
	}
	if len(e.UnmetDeps) > 0 {
		return fmt.Sprintf("%s cannot start: it depends on unfinished tasks %s. Finish or cancel them first, or remove the dependency.", task, joinIDs(EntityTask, e.UnmetDeps))
	}
	msg := fmt.Sprintf("%s cannot go from %s to %s.", task, e.From, e.To)
	if e.Reason != "" {
		msg += " " + e.Reason + "."
	}
	if next := AllowedTransitions(e.From); len(next) > 0 {
		names := make([]string, len(next))
		for i, s := range next {
			names[i] = string(s)
		}
		msg += fmt.Sprintf(" From %s it can only go to: %s.", e.From, strings.Join(names, ", "))
	}
	return msg
}

// ConflictError means an optimistic-concurrency check failed.
type ConflictError struct {
	Entity   EntityType
	ID       int64
	Expected int64
	Current  int64
	// CurrentTask is the task as it is now, when the conflict is about a task.
	// It lets callers show the current state without another read.
	CurrentTask *Task
}

func (e *ConflictError) Kind() ErrorKind { return KindConflict }
func (e *ConflictError) Error() string   { return e.Message() }
func (e *ConflictError) Is(t error) bool { _, ok := t.(*ConflictError); return ok }
func (e *ConflictError) Message() string {
	if t := e.CurrentTask; t != nil {
		return fmt.Sprintf("%s was changed by someone else (you expected version %d). It is now %s at version %d. Review the current state and retry with expected_version=%d.",
			FormatID(e.Entity, e.ID), e.Expected, t.Status, e.Current, e.Current)
	}
	return fmt.Sprintf("%s was changed by someone else (you expected version %d, current is %d). Re-read it and retry with expected_version=%d.",
		FormatID(e.Entity, e.ID), e.Expected, e.Current, e.Current)
}

// DependencyCycleError means a dependency would create a cycle. Path lists
// task IDs in order and ends with the task it started from.
type DependencyCycleError struct {
	Path []int64
}

func (e *DependencyCycleError) Kind() ErrorKind { return KindDependencyCycle }
func (e *DependencyCycleError) Error() string   { return e.Message() }
func (e *DependencyCycleError) Is(t error) bool {
	_, ok := t.(*DependencyCycleError)
	return ok
}
func (e *DependencyCycleError) Message() string {
	cycle := "a circular dependency"
	if len(e.Path) > 0 {
		parts := make([]string, len(e.Path))
		for i, id := range e.Path {
			parts[i] = FormatID(EntityTask, id)
		}
		cycle = strings.Join(parts, " -> ")
	}
	return fmt.Sprintf("This dependency would create a cycle: %s. Remove one dependency in the cycle or pick a different prerequisite.", cycle)
}

// EvidenceRequiredError means completing a task needs verification evidence.
type EvidenceRequiredError struct {
	TaskID int64
}

func (e *EvidenceRequiredError) Kind() ErrorKind { return KindEvidenceRequired }
func (e *EvidenceRequiredError) Error() string   { return e.Message() }
func (e *EvidenceRequiredError) Is(t error) bool {
	_, ok := t.(*EvidenceRequiredError)
	return ok
}
func (e *EvidenceRequiredError) Message() string {
	task := "The task"
	if e.TaskID != 0 {
		task = FormatID(EntityTask, e.TaskID)
	}
	return fmt.Sprintf("%s cannot be marked done without verification evidence. Record an artifact (commit, PR, test report, link or manual note) first, then complete it again. A human can override this with the force option.", task)
}

// ValidationError means input was malformed or out of range.
type ValidationError struct {
	Field   string
	Problem string
	Hint    string // what to do about it
}

func (e *ValidationError) Kind() ErrorKind { return KindValidation }
func (e *ValidationError) Error() string   { return e.Message() }
func (e *ValidationError) Is(t error) bool { _, ok := t.(*ValidationError); return ok }
func (e *ValidationError) Message() string {
	msg := e.Problem
	if e.Field != "" {
		msg = fmt.Sprintf("Invalid %s: %s", e.Field, e.Problem)
	}
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	if e.Hint != "" {
		msg += " " + e.Hint
	}
	return msg
}

func joinIDs(e EntityType, ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = FormatID(e, id)
	}
	return strings.Join(parts, ", ")
}

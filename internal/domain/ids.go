package domain

import (
	"fmt"
	"strconv"
	"strings"
)

var idPrefix = map[EntityType]string{
	EntityProject:  "P",
	EntityFeature:  "F",
	EntityTask:     "T",
	EntityArtifact: "A",
	EntityDocument: "D",
	EntityNote:     "N",
}

var prefixEntity = func() map[string]EntityType {
	m := make(map[string]EntityType, len(idPrefix))
	for e, p := range idPrefix {
		m[p] = e
	}
	return m
}()

// Prefix returns the short-ID prefix letter of e ("T" for tasks), or "" for
// an unknown entity type.
func Prefix(e EntityType) string { return idPrefix[e] }

// FormatID renders a database ID as a short ID such as "T-42".
func FormatID(e EntityType, id int64) string { return fmt.Sprintf("%s-%d", idPrefix[e], id) }

// ParseID parses a short ID for a known entity. It accepts "T-42", "t-42" and
// a bare "42". A prefix that belongs to another entity, zero, negative
// numbers, overflow and junk are rejected with a *ValidationError.
func ParseID(e EntityType, s string) (int64, error) {
	want, ok := idPrefix[e]
	if !ok {
		return 0, &ValidationError{Field: "id", Problem: fmt.Sprintf("unknown entity type %q", e)}
	}
	s = strings.TrimSpace(s)
	digits := s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		p := strings.ToUpper(s[:i])
		if p != want {
			if other, known := prefixEntity[p]; known {
				return 0, idError(s, e, fmt.Sprintf("%q is a %s ID, not a %s ID", s, other, e))
			}
			return 0, idError(s, e, "")
		}
		digits = s[i+1:]
	}
	n, ok := parseDigits(digits)
	if !ok {
		return 0, idError(s, e, "")
	}
	return n, nil
}

// ParseAnyID parses a short ID whose prefix says what it refers to, such as
// "F-7". A bare number is rejected because the entity is unknown.
func ParseAnyID(s string) (EntityType, int64, error) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, '-')
	if i < 0 {
		return "", 0, &ValidationError{Field: "id", Problem: fmt.Sprintf("%q has no prefix", s), Hint: "Use an ID with a prefix, like P-1, F-7 or T-42."}
	}
	e, ok := prefixEntity[strings.ToUpper(s[:i])]
	if !ok {
		return "", 0, &ValidationError{Field: "id", Problem: fmt.Sprintf("%q has an unknown prefix", s), Hint: "Use an ID with a prefix, like P-1, F-7 or T-42."}
	}
	n, ok := parseDigits(s[i+1:])
	if !ok {
		return "", 0, idError(s, e, "")
	}
	return e, n, nil
}

// parseDigits accepts only ASCII digits and a positive value.
func parseDigits(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func idError(s string, e EntityType, problem string) error {
	if problem == "" {
		problem = fmt.Sprintf("%q is not a valid %s ID", s, e)
	}
	return &ValidationError{
		Field:   "id",
		Problem: problem,
		Hint:    fmt.Sprintf("Use an ID like %s-42 (or just 42).", idPrefix[e]),
	}
}

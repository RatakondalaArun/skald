package domain

import (
	"fmt"
	"sort"
	"strings"
)

// Tag limits. Tags are short free-form labels such as "backend" or "ui:board".
const (
	MaxTagLength   = 32
	MaxTagsPerTask = 10
)

// NormalizeTag lowercases and trims a tag and checks it: 1 to MaxTagLength
// characters from a-z, 0-9 and "-", "_", ".", ":", "/", starting with a letter
// or digit.
func NormalizeTag(raw string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(raw))
	t = strings.TrimPrefix(t, "#")
	bad := func(problem string) error {
		return &ValidationError{Field: "tag", Problem: fmt.Sprintf("%q %s", raw, problem),
			Hint: fmt.Sprintf("Tags are up to %d characters of a-z, 0-9, '-', '_', '.', ':' or '/', such as \"backend\" or \"ui:board\".", MaxTagLength)}
	}
	if t == "" {
		return "", bad("is empty")
	}
	if len(t) > MaxTagLength {
		return "", bad(fmt.Sprintf("is longer than %d characters", MaxTagLength))
	}
	for i, r := range t {
		alnum := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if i == 0 && !alnum {
			return "", bad("must start with a letter or digit")
		}
		if !alnum && !strings.ContainsRune("-_.:/", r) {
			return "", bad(fmt.Sprintf("contains %q", r))
		}
	}
	return t, nil
}

// NormalizeTags normalizes each tag, drops duplicates and sorts the result.
// It never returns nil, so an empty set encodes as [].
func NormalizeTags(raw []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, r := range raw {
		t, err := NormalizeTag(r)
		if err != nil {
			return nil, err
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if len(out) > MaxTagsPerTask {
		return nil, &ValidationError{Field: "tags", Problem: fmt.Sprintf("has %d tags, more than %d", len(out), MaxTagsPerTask), Hint: "Use fewer, broader tags."}
	}
	sort.Strings(out)
	return out, nil
}

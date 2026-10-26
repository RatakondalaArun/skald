package domain

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeTag(t *testing.T) {
	for in, want := range map[string]string{
		"backend": "backend", "  UI ": "ui", "#bug": "bug", "ui:board": "ui:board", "v1.2": "v1.2", "a/b_c-d": "a/b_c-d",
		strings.Repeat("x", MaxTagLength): strings.Repeat("x", MaxTagLength),
	} {
		got, err := NormalizeTag(in)
		if err != nil || got != want {
			t.Errorf("NormalizeTag(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "  ", "#", "-lead", "two words", "emoji✓", "semi;colon", strings.Repeat("x", MaxTagLength+1)} {
		if _, err := NormalizeTag(in); !errors.Is(err, ErrValidation) {
			t.Errorf("NormalizeTag(%q) err = %v, want a validation error", in, err)
		}
	}
}

func TestNormalizeTagsDedupesAndSorts(t *testing.T) {
	got, err := NormalizeTags([]string{"ui", "Backend", "UI", "api"})
	if err != nil || !slices.Equal(got, []string{"api", "backend", "ui"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := NormalizeTags(nil); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("nil input: got %#v, %v; want an empty non-nil slice", got, err)
	}
	many := make([]string, MaxTagsPerTask+1)
	for i := range many {
		many[i] = string(rune('a' + i))
	}
	if _, err := NormalizeTags(many); !errors.Is(err, ErrValidation) {
		t.Fatalf("too many tags: err = %v", err)
	}
	// Duplicates do not count towards the limit.
	if _, err := NormalizeTags(append(many[:MaxTagsPerTask], "A")); err != nil {
		t.Fatalf("duplicates counted: %v", err)
	}
}

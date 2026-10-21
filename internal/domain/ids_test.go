package domain

import (
	"strings"
	"testing"
)

func TestFormatParseRoundTrip(t *testing.T) {
	for _, e := range []EntityType{EntityProject, EntityFeature, EntityTask, EntityArtifact, EntityDocument, EntityNote} {
		for _, id := range []int64{1, 7, 42, 1 << 40} {
			s := FormatID(e, id)
			got, err := ParseID(e, s)
			if err != nil || got != id {
				t.Errorf("ParseID(%s, %q) = %d, %v; want %d", e, s, got, err, id)
			}
			ge, gid, err := ParseAnyID(s)
			if err != nil || ge != e || gid != id {
				t.Errorf("ParseAnyID(%q) = %s, %d, %v", s, ge, gid, err)
			}
		}
	}
	if got := FormatID(EntityTask, 42); got != "T-42" {
		t.Errorf("FormatID = %q", got)
	}
	if Prefix(EntityFeature) != "F" || Prefix(EntityType("x")) != "" {
		t.Error("Prefix wrong")
	}
}

func TestParseIDAccepts(t *testing.T) {
	tests := []struct {
		e    EntityType
		in   string
		want int64
	}{
		{EntityTask, "T-42", 42},
		{EntityTask, "t-42", 42},
		{EntityTask, "42", 42},
		{EntityTask, "  T-42 \n", 42},
		{EntityTask, "T-007", 7},
		{EntityProject, "p-1", 1},
		{EntityFeature, "F-9223372036854775807", 9223372036854775807},
	}
	for _, tt := range tests {
		got, err := ParseID(tt.e, tt.in)
		if err != nil || got != tt.want {
			t.Errorf("ParseID(%s, %q) = %d, %v; want %d", tt.e, tt.in, got, err, tt.want)
		}
	}
}

func TestParseIDRejects(t *testing.T) {
	tests := []struct {
		name string
		e    EntityType
		in   string
		msg  string
	}{
		{"wrong prefix", EntityTask, "F-3", "feature ID, not a task ID"},
		{"unknown prefix", EntityTask, "X-3", ""},
		{"zero", EntityTask, "T-0", ""},
		{"bare zero", EntityTask, "0", ""},
		{"negative", EntityTask, "T--5", ""},
		{"bare negative", EntityTask, "-5", ""},
		{"plus sign", EntityTask, "T-+5", ""},
		{"empty", EntityTask, "", ""},
		{"prefix only", EntityTask, "T-", ""},
		{"no digits", EntityTask, "T-abc", ""},
		{"junk suffix", EntityTask, "T-12x", ""},
		{"float", EntityTask, "T-1.5", ""},
		{"space inside", EntityTask, "T- 5", ""},
		{"overflow", EntityTask, "T-9223372036854775808", ""},
		{"no hyphen", EntityTask, "T42", ""},
		{"unknown entity", EntityType("nope"), "1", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseID(tt.e, tt.in)
			if KindOf(err) != KindValidation {
				t.Fatalf("want ValidationError, got %v", err)
			}
			if tt.msg != "" && !strings.Contains(err.Error(), tt.msg) {
				t.Errorf("message %q lacks %q", err.Error(), tt.msg)
			}
		})
	}
}

func TestParseAnyIDRejects(t *testing.T) {
	for _, in := range []string{"42", "", "X-1", "T-0", "T-", "T-1x", "-1"} {
		if _, _, err := ParseAnyID(in); KindOf(err) != KindValidation {
			t.Errorf("ParseAnyID(%q) = %v, want ValidationError", in, err)
		}
	}
}

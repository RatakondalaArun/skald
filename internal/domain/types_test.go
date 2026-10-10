package domain

import "testing"

func TestStatusValidity(t *testing.T) {
	for _, s := range allStatuses {
		if !s.Valid() {
			t.Errorf("%s should be valid", s)
		}
	}
	if TaskStatus("x").Valid() || TaskStatus("").Valid() {
		t.Error("bad task status valid")
	}
	if !TaskDone.Terminal() || !TaskCancelled.Terminal() || TaskTodo.Terminal() || TaskBlocked.Terminal() || TaskInProgress.Terminal() {
		t.Error("Terminal wrong")
	}
	if !ProjectActive.Valid() || !ProjectArchived.Valid() || ProjectStatus("x").Valid() {
		t.Error("project status")
	}
	for _, s := range []FeatureStatus{FeaturePlanned, FeatureActive, FeatureDone, FeatureCancelled} {
		if !s.Valid() {
			t.Errorf("feature %s", s)
		}
	}
	if FeatureStatus("x").Valid() {
		t.Error("feature status")
	}
	for _, k := range []ArtifactKind{ArtifactCommit, ArtifactPR, ArtifactTestReport, ArtifactLink, ArtifactManual} {
		if !k.Valid() {
			t.Errorf("artifact %s", k)
		}
	}
	if ArtifactKind("diff").Valid() {
		t.Error("artifact kind")
	}
	if !EntityTask.Valid() || EntityType("x").Valid() {
		t.Error("entity type")
	}
	if !EventTaskStatus.Valid() || EventType("task.exploded").Valid() {
		t.Error("event type")
	}
}

func TestActor(t *testing.T) {
	a := NewAgentActor("claude-code", "a1b2")
	if a != "claude:claude-code/a1b2" || !a.IsAgent() || a.IsHuman() || !a.Valid() {
		t.Errorf("agent actor %q", a)
	}
	if got := NewAgentActor("My Client/x", ""); got != "claude:My-Client-x/unknown" {
		t.Errorf("sanitised actor %q", got)
	}
	if !Human.IsHuman() || Human.IsAgent() || !Human.Valid() {
		t.Error("human actor")
	}
	if Actor("").Valid() || Actor("a b").Valid() || Actor("").IsHuman() {
		t.Error("invalid actors accepted")
	}
}

func TestParseSettings(t *testing.T) {
	s, err := ParseSettings("")
	if err != nil || !s.RequireEvidence {
		t.Errorf("empty = %+v, %v", s, err)
	}
	s, err = ParseSettings("{}")
	if err != nil || !s.RequireEvidence {
		t.Errorf("{} should keep default true: %+v, %v", s, err)
	}
	s, err = ParseSettings(`{"require_evidence": false}`)
	if err != nil || s.RequireEvidence {
		t.Errorf("explicit false: %+v, %v", s, err)
	}
	if _, err = ParseSettings("not json"); KindOf(err) != KindValidation {
		t.Errorf("bad json err = %v", err)
	}
}

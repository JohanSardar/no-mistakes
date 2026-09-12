package types

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseWorkerProvenance_EmptyIsUnknown(t *testing.T) {
	p, err := ParseWorkerProvenance("")
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Fatalf("empty input = %+v, want nil (unknown)", p)
	}
	id := WorkerIdentityFrom(nil)
	if id.Tool != WorkerFieldUnknown || id.Model != WorkerFieldUnknown || id.Provider != WorkerFieldUnknown {
		t.Fatalf("unknown identity = %+v", id)
	}
}

func TestParseWorkerProvenance_KeepsSuppliedFieldsOnly(t *testing.T) {
	p, err := ParseWorkerProvenance(`{"task_id":"task-9","tool":"grok","model":"grok-4.6","provider":"xai","settings":{"effort":"high"},"ignored":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || p.TaskID != "task-9" || p.Tool != "grok" || p.Model != "grok-4.6" || p.Provider != "xai" {
		t.Fatalf("parsed = %+v", p)
	}
	var settings map[string]string
	if err := json.Unmarshal(p.Settings, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["effort"] != "high" {
		t.Fatalf("settings = %v", settings)
	}
	id := WorkerIdentityFrom(p)
	if id.Tool != "grok" || id.Model != "grok-4.6" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestParseWorkerProvenance_RejectsInvalidJSON(t *testing.T) {
	if _, err := ParseWorkerProvenance("{"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestParseWorkerProvenance_DropsNonObjectSettings(t *testing.T) {
	p, err := ParseWorkerProvenance(`{"tool":"codex","settings":"not-an-object"}`)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || p.Tool != "codex" {
		t.Fatalf("parsed = %+v", p)
	}
	if len(p.Settings) != 0 {
		t.Fatalf("settings should be dropped, got %s", p.Settings)
	}
}

func TestLaunchAttribution_FixesRunIDIsTheBugFixSignal(t *testing.T) {
	var empty LaunchAttribution
	if empty.IsBugFixRun() {
		t.Fatal("empty launch is not a bug-fix run")
	}
	linked := LaunchAttribution{FixesRunID: " run-abc "}
	linked.Normalize()
	if !linked.IsBugFixRun() || linked.FixesRunID != "run-abc" {
		t.Fatalf("typed signal not recognized: %+v", linked)
	}
}

func TestRecalculateAttributionCounts(t *testing.T) {
	rec := &AttributionRecord{
		Bugs: []AttributedBug{
			{Attribution: AttributionOriginalWorker, Outcome: BugOutcomeFixedBeforeShipping},
			{Attribution: AttributionPipeline, Outcome: BugOutcomeFixedBeforeShipping},
			{Attribution: AttributionPreExisting, Outcome: BugOutcomeStillOpen},
			{Attribution: AttributionUnknown, Outcome: BugOutcomeEscaped},
		},
		NonBugs: []NonBugChange{{Kind: ChangeKindDocs}},
	}
	RecalculateAttributionCounts(rec)
	if rec.Counts.OriginalWorker != 1 || rec.Counts.Pipeline != 1 || rec.Counts.PreExisting != 1 || rec.Counts.Unknown != 1 {
		t.Fatalf("bucket counts = %+v", rec.Counts)
	}
	if rec.Counts.FixedBeforeShipping != 2 || rec.Counts.Escaped != 1 || rec.Counts.NonBugs != 1 {
		t.Fatalf("outcome counts = %+v", rec.Counts)
	}
	if strings.Contains(strings.Join(DefaultAttributionLimits(), "\n"), "blame") == false {
		t.Fatal("limits should document the blame prohibition")
	}
}

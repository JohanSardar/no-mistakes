package db

import (
	"testing"
)

func TestRunInsertHasNoAttribution(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkerProvenanceJSON != nil || got.FixesRunID != nil || got.Rerun || got.AttributionSnapshotJSON != nil || got.AttributionJSON != nil {
		t.Fatalf("legacy insert grew attribution fields: %+v", got)
	}
}

func TestSetRunLaunchAttributionLeavesOriginatingRunUntouched(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	original, err := d.InsertRun(repo.ID, "feature", "aaa", "bbb")
	if err != nil {
		t.Fatal(err)
	}
	fix, err := d.InsertRun(repo.ID, "hotfix", "ccc", "ddd")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunLaunchAttribution(fix.ID, `{"tool":"grok","model":"grok-4.6"}`, original.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(fix.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkerProvenanceJSON == nil || *got.WorkerProvenanceJSON != `{"tool":"grok","model":"grok-4.6"}` {
		t.Fatalf("worker provenance = %v", got.WorkerProvenanceJSON)
	}
	if got.FixesRunID == nil || *got.FixesRunID != original.ID {
		t.Fatalf("fixes_run_id = %v", got.FixesRunID)
	}
	if got.Rerun {
		t.Fatal("a pushed run was marked as a rerun")
	}
	// Originating run is unchanged.
	orig, err := d.GetRun(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if orig.FixesRunID != nil || orig.AttributionJSON != nil {
		t.Fatalf("originating run was rewritten: %+v", orig)
	}
}

func TestSetRunLaunchAttributionRecordsTheRerunMarker(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, err := d.InsertRun(repo.ID, "feature", "aaa", "bbb")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunLaunchAttribution(run.ID, "", "", true); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Rerun {
		t.Fatal("rerun marker was not persisted")
	}
	if got.WorkerProvenanceJSON != nil || got.FixesRunID != nil {
		t.Fatalf("rerun marker invented provenance: %+v", got)
	}
}

func TestAttributionSnapshotIsNotRewrittenByFinal(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunAttributionSnapshot(run.ID, `{"phase":"snapshot","status":"complete"}`); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunAttributionFinal(run.ID, `{"phase":"final","status":"complete","reconciled":true}`); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AttributionSnapshotJSON == nil || *got.AttributionSnapshotJSON != `{"phase":"snapshot","status":"complete"}` {
		t.Fatalf("snapshot rewritten: %v", got.AttributionSnapshotJSON)
	}
	if got.AttributionJSON == nil || *got.AttributionJSON != `{"phase":"final","status":"complete","reconciled":true}` {
		t.Fatalf("final = %v", got.AttributionJSON)
	}
}

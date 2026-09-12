package attribution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const zeroSHA = "0000000000000000000000000000000000000000"

func TestMain(m *testing.M) {
	os.Unsetenv("GIT_CONFIG_COUNT")
	os.Exit(m.Run())
}

func TestAddedLinesIgnoresContextAndDeletions(t *testing.T) {
	diff := `diff --git a/foo.go b/foo.go
--- a/foo.go
+++ b/foo.go
@@ -1,4 +1,5 @@
 package foo
+func added() {}
 func existed() {}
-func removed() {}
 func other() {}
`
	got := addedLines(diff)
	if _, ok := got["foo.go"][2]; !ok {
		t.Fatalf("expected added line 2, got %v", got["foo.go"])
	}
	if _, ok := got["foo.go"][3]; ok {
		t.Fatal("context line must not count as added")
	}
}

// Inside a hunk every "+" line is content: a prefix-increment statement
// ("+++count;") or an added line beginning with "++ " is an addition, not a
// file header that resets the path or the line counter.
func TestAddedLinesCountsPlusPrefixedContentInsideAHunk(t *testing.T) {
	diff := `diff --git a/svc.c b/svc.c
--- a/svc.c
+++ b/svc.c
@@ -4,1 +4,5 @@
 int count = 0;
+++count;
+buf[count] = x;
+++ x
+done();
diff --git a/other.c b/other.c
--- a/other.c
+++ b/other.c
@@ -0,0 +1 @@
+int other;
`
	got := addedLines(diff)
	for _, want := range []int{5, 6, 7, 8} {
		if _, ok := got["svc.c"][want]; !ok {
			t.Fatalf("svc.c line %d missing from %v", want, got["svc.c"])
		}
	}
	if _, ok := got["other.c"][1]; !ok {
		t.Fatalf("other.c line 1 missing from %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("a content line was read as a file header: %v", got)
	}
}

func TestSnapshot_OriginalWorkerFix(t *testing.T) {
	dir, _, submitted, pipelineHead := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, pipelineHead)
	seedReviewFinding(t, in, "handler.go", 2, "nil deref", types.ActionAutoFix, true, submitted)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != types.AttributionStatusComplete {
		t.Fatalf("status = %s gaps=%v", rec.Status, rec.EvidenceGaps)
	}
	if rec.Phase != types.AttributionPhaseSnapshot {
		t.Fatalf("phase = %s", rec.Phase)
	}
	if rec.SubmittedHeadSHA != submitted {
		t.Fatalf("submitted = %s want %s", rec.SubmittedHeadSHA, submitted)
	}
	if rec.SnapshotHeadSHA != pipelineHead {
		t.Fatalf("snapshot head = %s want %s", rec.SnapshotHeadSHA, pipelineHead)
	}
	if rec.Counts.OriginalWorker != 1 || rec.Counts.FixedBeforeShipping != 1 {
		t.Fatalf("counts = %+v bugs=%+v gaps=%v", rec.Counts, rec.Bugs, rec.EvidenceGaps)
	}
	if rec.Bugs[0].Attribution != types.AttributionOriginalWorker || rec.Bugs[0].Confidence != types.AttributionConfidenceHigh {
		t.Fatalf("attribution = %s/%s evidence=%v", rec.Bugs[0].Attribution, rec.Bugs[0].Confidence, rec.Bugs[0].Evidence)
	}
	if rec.Bugs[0].Outcome != types.BugOutcomeFixedBeforeShipping {
		t.Fatalf("outcome = %s", rec.Bugs[0].Outcome)
	}
}

// The daemon never stores the branch base in Run.BaseSHA: a first push
// records the zero SHA, an axi-run fallback records the submitted head itself,
// and a second push records only the previous gate head. None of them may
// change the worker attribution.
func TestSnapshot_DaemonBaseSHAValuesDoNotChangeAttribution(t *testing.T) {
	for _, tc := range []struct {
		name string
		base func(base, submitted string) string
	}{
		{"zero_sha_first_push", func(_, _ string) string { return zeroSHA }},
		{"submitted_head_fresh_launch", func(_, submitted string) string { return submitted }},
		{"branch_base", func(base, _ string) string { return base }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, submitted, head := repoWithWorkerBugAndPipelineFix(t)
			in := fixtureInput(t, dir, submitted, head)
			in.Run.BaseSHA = tc.base(base, submitted)
			seedReviewFinding(t, in, "handler.go", 2, "nil deref", types.ActionAutoFix, true, submitted)
			rec, err := Snapshot(context.Background(), in.Input)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Counts.OriginalWorker != 1 || rec.Status != types.AttributionStatusComplete {
				t.Fatalf("counts = %+v status=%s bugs=%+v gaps=%v", rec.Counts, rec.Status, rec.Bugs, rec.EvidenceGaps)
			}
		})
	}
}

// A second push of the same branch: the worker's earlier commits are still
// the worker's, not pre-existing code.
func TestSnapshot_EarlierWorkerCommitsAreNotPreExisting(t *testing.T) {
	dir, _, first, _ := repoWithWorkerBugAndPipelineFix(t)
	gitCmd(t, dir, "reset", "--hard", first)
	second := commitFile(t, dir, "second.go", "package handler\nfunc later() {}\n", "worker second push")
	head := commitFile(t, dir, "handler.go", "package handler\nfunc worker() {}\nfunc pipelineFix() {}\n", "pipeline fix")
	in := fixtureInput(t, dir, second, head)
	in.Run.BaseSHA = first
	seedReviewFinding(t, in, "handler.go", 2, "nil deref", types.ActionAutoFix, true, second)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.OriginalWorker != 1 || rec.Counts.PreExisting != 0 {
		t.Fatalf("counts = %+v bugs=%+v gaps=%v", rec.Counts, rec.Bugs, rec.EvidenceGaps)
	}
}

func TestSnapshot_PipelineIntroducedFix(t *testing.T) {
	dir, _, submitted, pipelineHead := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, pipelineHead)
	// Finding line is the pipeline-added line (func pipelineFix).
	seedReviewFinding(t, in, "handler.go", 3, "pipeline introduced panic", types.ActionAutoFix, true, pipelineHead)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.Pipeline != 1 {
		t.Fatalf("counts = %+v bugs=%+v gaps=%v", rec.Counts, rec.Bugs, rec.EvidenceGaps)
	}
	if rec.Bugs[0].Attribution != types.AttributionPipeline {
		t.Fatalf("attribution = %s evidence=%v", rec.Bugs[0].Attribution, rec.Bugs[0].Evidence)
	}
}

// The flagship case: the fix round replaces the buggy line in place, and the
// re-review finds a new defect at the same file:line. Each finding is located
// at the commit its own round examined, so the worker's original bug and the
// pipeline-introduced one are told apart instead of both reading "unknown".
func TestSnapshot_InPlaceFixLocatesEachFindingAtItsOwnRound(t *testing.T) {
	dir, _, submitted, _ := repoWithWorkerBugAndPipelineFix(t)
	gitCmd(t, dir, "reset", "--hard", submitted)
	head := commitFile(t, dir, "handler.go", "package handler\nfunc worker() { fixed() }\n", "pipeline in-place fix")
	in := fixtureInput(t, dir, submitted, head)
	review := insertStep(t, in, types.StepReview, types.StepStatusCompleted)
	original := findingsJSON(types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 2, Description: "nil deref", Action: types.ActionAutoFix})
	introduced := findingsJSON(types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 2, Description: "fixed() ignores its error", Action: types.ActionAutoFix})
	insertRound(t, in, review, 1, "initial", original, submitted, true, false)
	insertRound(t, in, review, 2, "auto_fix", introduced, head, false, true)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.OriginalWorker != 1 || rec.Counts.Pipeline != 1 || rec.Counts.Unknown != 0 {
		t.Fatalf("counts = %+v bugs=%+v gaps=%v", rec.Counts, rec.Bugs, rec.EvidenceGaps)
	}
	byDesc := bugsByDescription(rec)
	// The final round still reports handler.go:2, so the worker bug is not
	// credited as fixed: the re-report may be the same defect reworded.
	if got := byDesc["nil deref"]; got.Attribution != types.AttributionOriginalWorker || got.Outcome != types.BugOutcomeUnknown {
		t.Fatalf("worker bug = %+v", got)
	}
	if got := byDesc["fixed() ignores its error"]; got.Attribution != types.AttributionPipeline || got.Outcome != types.BugOutcomeStillOpen {
		t.Fatalf("pipeline bug = %+v", got)
	}
	if rec.Counts.FixedBeforeShipping != 0 {
		t.Fatalf("credited a fix the final round contradicts: %+v", rec.Counts)
	}
}

// The Test step records no ReviewedHeadSHA on its rounds; the head each
// round validated is the tested_head_sha inside its findings, and a finding
// is located there, not at the post-fix head.
func TestSnapshot_TestStepInPlaceFixLocatesEachFindingAtItsTestedHead(t *testing.T) {
	dir, _, submitted, _ := repoWithWorkerBugAndPipelineFix(t)
	gitCmd(t, dir, "reset", "--hard", submitted)
	head := commitFile(t, dir, "handler.go", "package handler\nfunc worker() { fixed() }\n", "pipeline in-place fix")
	in := fixtureInput(t, dir, submitted, head)
	test := insertStep(t, in, types.StepTest, types.StepStatusCompleted)
	original := testedFindingsJSON(submitted, types.Finding{ID: "test-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 2, Description: "worker() panics on nil input", Action: types.ActionAutoFix})
	introduced := testedFindingsJSON(head, types.Finding{ID: "test-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 2, Description: "fixed() ignores its error", Action: types.ActionAutoFix})
	insertRound(t, in, test, 1, "initial", original, "", true, false)
	insertRound(t, in, test, 2, "auto_fix", introduced, "", false, true)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.OriginalWorker != 1 || rec.Counts.Pipeline != 1 || rec.Counts.Unknown != 0 {
		t.Fatalf("counts = %+v bugs=%+v gaps=%v", rec.Counts, rec.Bugs, rec.EvidenceGaps)
	}
	byDesc := bugsByDescription(rec)
	if got := byDesc["worker() panics on nil input"]; got.Attribution != types.AttributionOriginalWorker || got.Confidence != types.AttributionConfidenceHigh {
		t.Fatalf("worker bug = %+v", got)
	}
	if got := byDesc["fixed() ignores its error"]; got.Attribution != types.AttributionPipeline {
		t.Fatalf("pipeline bug = %+v", got)
	}
}

// A defect the re-review still reports at the same file:line under different
// wording is not evidence the fix landed: the first report stays unknown, the
// second stays open, and nothing is credited as fixed before shipping.
func TestSnapshot_RewordedFindingAtTheSameLocationIsNotCreditedAsFixed(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	review := insertStep(t, in, types.StepReview, types.StepStatusCompleted)
	first := findingsJSON(types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 2, Description: "nil deref of svc", Action: types.ActionAutoFix})
	reworded := findingsJSON(types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 2, Description: "svc may be nil here", Action: types.ActionAutoFix})
	insertRound(t, in, review, 1, "initial", first, submitted, true, false)
	insertRound(t, in, review, 2, "auto_fix", reworded, head, false, true)

	snap, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.FixedBeforeShipping != 0 {
		t.Fatalf("credited a fix the re-review contradicts: %+v", snap.Bugs)
	}
	byDesc := bugsByDescription(snap)
	if got := byDesc["nil deref of svc"]; got.Outcome != types.BugOutcomeUnknown {
		t.Fatalf("first report = %+v", got)
	}
	if got := byDesc["svc may be nil here"]; got.Outcome != types.BugOutcomeStillOpen {
		t.Fatalf("re-report = %+v", got)
	}

	in.Run.Status = types.RunCompleted
	final, err := Reconcile(context.Background(), in.Input, snap)
	if err != nil {
		t.Fatal(err)
	}
	if final.Counts.FixedBeforeShipping != 0 || final.Counts.Escaped != 1 {
		t.Fatalf("final counts = %+v bugs=%+v", final.Counts, final.Bugs)
	}
}

// A human-authored finding selected at the fix gate is a confirmed bug: it is
// read from the round's user findings and attributed like any other.
func TestSnapshot_UserAuthoredFindingSelectedForFixIsABug(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	review := insertStep(t, in, types.StepReview, types.StepStatusCompleted)
	clean := findingsJSON()
	round := insertRound(t, in, review, 1, "initial", clean, submitted, false, false)
	merged := types.MergeUserOverrides(types.Findings{Summary: "0 selected findings"}, nil, []types.Finding{{
		File: "handler.go", Line: 2, Description: "worker() drops the request context",
	}})
	mergedJSON, err := types.MarshalFindingsJSON(merged)
	if err != nil {
		t.Fatal(err)
	}
	ids := `["` + merged.Items[0].ID + `"]`
	if err := in.DB.SetStepRoundUserDecision(round.ID, &ids, db.RoundSelectionSourceUser, &mergedJSON); err != nil {
		t.Fatal(err)
	}
	round.SelectedFindingIDs, round.UserFindingsJSON = &ids, &mergedJSON
	insertRound(t, in, review, 2, "user_fix", clean, head, false, true)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Bugs) != 1 || rec.Counts.OriginalWorker != 1 || rec.Counts.FixedBeforeShipping != 1 {
		t.Fatalf("user finding not attributed: bugs=%+v counts=%+v non_bugs=%+v", rec.Bugs, rec.Counts, rec.NonBugs)
	}
	if rec.Bugs[0].ID != merged.Items[0].ID || rec.Bugs[0].Outcome != types.BugOutcomeFixedBeforeShipping {
		t.Fatalf("bug = %+v", rec.Bugs[0])
	}
}

// Gate agents address files by absolute worktree path; the path is matched
// relative to the worktree, and one outside it is unknown, never pre-existing.
func TestSnapshot_AbsoluteFindingPathIsMatchedRelativeToTheWorktree(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	seedReviewFinding(t, in, filepath.Join(dir, "handler.go"), 2, "nil deref", types.ActionAutoFix, true, submitted)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.OriginalWorker != 1 || rec.Bugs[0].Attribution != types.AttributionOriginalWorker {
		t.Fatalf("absolute path lost its diff match: %+v", rec.Bugs)
	}
}

func TestSnapshot_FindingPathOutsideTheWorktreeIsUnknown(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	seedReviewFinding(t, in, filepath.Join(t.TempDir(), "handler.go"), 2, "nil deref", types.ActionAutoFix, true, submitted)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.Unknown != 1 || rec.Counts.PreExisting != 0 || rec.Bugs[0].Attribution != types.AttributionUnknown {
		t.Fatalf("foreign path attributed: %+v", rec.Bugs)
	}
	if len(rec.Bugs[0].Evidence) == 0 || !strings.Contains(rec.Bugs[0].Evidence[0], "outside the worktree") {
		t.Fatalf("evidence = %v", rec.Bugs[0].Evidence)
	}
}

// Session-free review rounds report the same file in whichever form they
// like; an absolute path in one round and a relative one in the next name one
// finding, recorded worktree-relative, not a fixed bug plus an escaped one.
func TestSnapshot_PathFormDoesNotSplitAFinding(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	review := insertStep(t, in, types.StepReview, types.StepStatusCompleted)
	absolute := findingsJSON(types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: filepath.Join(dir, "handler.go"), Line: 2, Description: "nil deref", Action: types.ActionAutoFix})
	relative := findingsJSON(types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: "./handler.go", Line: 2, Description: "nil deref", Action: types.ActionAutoFix})
	insertRound(t, in, review, 1, "initial", absolute, submitted, true, false)
	insertRound(t, in, review, 2, "auto_fix", relative, head, false, true)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Bugs) != 1 || rec.Counts.FixedBeforeShipping != 0 || rec.Counts.OriginalWorker != 1 {
		t.Fatalf("path form split the finding: bugs=%+v counts=%+v", rec.Bugs, rec.Counts)
	}
	if rec.Bugs[0].File != "handler.go" || rec.Bugs[0].Outcome != types.BugOutcomeStillOpen {
		t.Fatalf("bug = %+v", rec.Bugs[0])
	}
}

// A run that fails before reaching Attribution still records a final record
// on the run (unavailable, never a clean score) but writes no findings onto a
// step that never ran; a skipped step is labelled final like every other
// stored final record.
func TestReconcileRun_PendingStepKeepsNoFindings(t *testing.T) {
	for _, tc := range []struct {
		status       types.StepStatus
		wantStatus   string
		wantFindings bool
	}{
		{types.StepStatusPending, types.AttributionStatusUnavailable, false},
		{types.StepStatusSkipped, types.AttributionStatusSkipped, true},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
			in := fixtureInput(t, dir, submitted, head)
			attr := insertStep(t, in, types.StepAttribution, tc.status)
			in.Run.Status = types.RunFailed

			if err := ReconcileRun(context.Background(), in.DB, in.Run, in.Repo, dir); err != nil {
				t.Fatal(err)
			}
			if in.Run.AttributionJSON == nil {
				t.Fatal("final record was not written to the run")
			}
			rec, err := UnmarshalRecord(*in.Run.AttributionJSON)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Status != tc.wantStatus || rec.Phase != types.AttributionPhaseFinal {
				t.Fatalf("record = %s/%s, want %s/final", rec.Status, rec.Phase, tc.wantStatus)
			}
			steps, err := in.DB.GetStepsByRun(in.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range steps {
				if step.ID != attr.ID {
					continue
				}
				if (step.FindingsJSON != nil) != tc.wantFindings {
					t.Fatalf("attribution step findings = %v, want present=%v", step.FindingsJSON, tc.wantFindings)
				}
			}
		})
	}
}

func TestSnapshot_PreExistingNotWorker(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	seedReviewFinding(t, in, "base.go", 2, "pre-existing off-by-one", types.ActionAutoFix, true, submitted)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.PreExisting != 1 {
		t.Fatalf("counts = %+v bugs=%+v", rec.Counts, rec.Bugs)
	}
	if rec.Bugs[0].Attribution != types.AttributionPreExisting {
		t.Fatalf("attribution = %s", rec.Bugs[0].Attribution)
	}
}

// Same-named files in different directories are different files: a finding in
// an untouched internal/config/config.go must not match the worker's new
// internal/a/config.go.
func TestSnapshot_BasenameIsNotFileIdentity(t *testing.T) {
	dir, _, _, _ := repoWithWorkerBugAndPipelineFix(t)
	gitCmd(t, dir, "checkout", "main")
	if err := os.MkdirAll(filepath.Join(dir, "internal", "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	commitFile(t, dir, "internal/config/config.go", "package config\nfunc Load() {}\n", "pre-existing config")
	gitCmd(t, dir, "checkout", "-b", "feature2")
	if err := os.MkdirAll(filepath.Join(dir, "internal", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	submitted := commitFile(t, dir, "internal/a/config.go", "package a\nfunc New() {}\n", "worker adds another config.go")
	in := fixtureInput(t, dir, submitted, submitted)
	seedReviewFinding(t, in, "internal/config/config.go", 2, "Load ignores errors", types.ActionAutoFix, true, submitted)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.OriginalWorker != 0 || rec.Counts.PreExisting != 1 {
		t.Fatalf("basename matched an untouched file: %+v %+v", rec.Counts, rec.Bugs)
	}
}

// The engine's diffs must read the same under a maintainer's git config that
// reshapes ordinary `git diff` output.
func TestSnapshot_IgnoresMaintainerDiffConfig(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	gitCmd(t, dir, "config", "diff.external", "/bin/false")
	gitCmd(t, dir, "config", "diff.mnemonicPrefix", "true")
	gitCmd(t, dir, "config", "diff.noprefix", "true")
	gitCmd(t, dir, "config", "color.ui", "always")
	gitCmd(t, dir, "config", "color.diff", "always")
	gitCmd(t, dir, "config", "diff.context", "0")
	in := fixtureInput(t, dir, submitted, head)
	seedReviewFinding(t, in, "handler.go", 2, "nil deref", types.ActionAutoFix, true, submitted)
	review := in.Steps[len(in.Steps)-1]
	insertRound(t, in, review, 3, "initial", findingsJSON(types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 3, Description: "pipeline panic", Action: types.ActionAutoFix}), head, false, false)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.OriginalWorker != 1 || rec.Counts.Pipeline != 1 || rec.Counts.PreExisting != 0 {
		t.Fatalf("counts = %+v bugs=%+v gaps=%v", rec.Counts, rec.Bugs, rec.EvidenceGaps)
	}
}

func TestSnapshot_UnknownWithoutFileLine(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	seedReviewFinding(t, in, "", 0, "something is wrong somewhere", types.ActionAutoFix, true, submitted)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.Unknown != 1 {
		t.Fatalf("counts = %+v bugs=%+v", rec.Counts, rec.Bugs)
	}
}

func TestSnapshot_NonBugChangesNotCounted(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	doc := insertStep(t, in, types.StepDocument, types.StepStatusCompleted)
	findings := findingsJSON(types.Finding{
		ID: "doc-1", Severity: types.FindingSeverityWarning, File: "README.md", Line: 1,
		Description: "docs stale", Action: types.ActionAutoFix, Category: types.FindingCategoryDocumentation,
	})
	insertRound(t, in, doc, 1, "initial", findings, submitted, false, false)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.OriginalWorker+rec.Counts.Pipeline+rec.Counts.PreExisting+rec.Counts.Unknown != 0 {
		t.Fatalf("docs counted as bugs: %+v %+v", rec.Counts, rec.Bugs)
	}
	if rec.Counts.NonBugs != 1 {
		t.Fatalf("non_bugs = %d", rec.Counts.NonBugs)
	}
}

func TestSnapshot_AskUserWithoutFixIsNotABug(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	seedReviewFinding(t, in, "handler.go", 2, "should we redesign this", types.ActionAskUser, false, submitted)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.OriginalWorker != 0 {
		t.Fatalf("ask-user counted as bug: %+v", rec.Bugs)
	}
	if rec.Counts.NonBugs != 1 {
		t.Fatalf("non_bugs = %d", rec.Counts.NonBugs)
	}
}

func TestSnapshot_AskUserSelectedByUserFixIsABug(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	review := insertStep(t, in, types.StepReview, types.StepStatusCompleted)
	f := findingsJSON(types.Finding{ID: "review-1", Severity: types.FindingSeverityWarning, File: "handler.go", Line: 2, Description: "races on shutdown", Action: types.ActionAskUser})
	insertRound(t, in, review, 1, "initial", f, submitted, true, false)
	insertRound(t, in, review, 2, "auto_fix", "", head, false, true)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.OriginalWorker != 1 || rec.Counts.FixedBeforeShipping != 1 || rec.Counts.NonBugs != 0 {
		t.Fatalf("counts = %+v bugs=%+v", rec.Counts, rec.Bugs)
	}
}

func TestSnapshot_DiffAloneIsNotABug(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	insertStep(t, in, types.StepReview, types.StepStatusCompleted)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.OriginalWorker != 0 || len(rec.Bugs) != 0 {
		t.Fatalf("diff was treated as a bug: %+v", rec.Bugs)
	}
}

// Finding IDs are positional per round: `review-1` in the fix round's
// re-review is a different finding from `review-1` in round 1. Identity is
// content, and each round's selection applies to that round's own findings.
func TestSnapshot_RoundIDsDoNotCollideAcrossRounds(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	review := insertStep(t, in, types.StepReview, types.StepStatusCompleted)
	a := findingsJSON(types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 2, Description: "nil deref", Action: types.ActionAutoFix})
	b := findingsJSON(types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 3, Description: "fix introduced unchecked error", Action: types.ActionAutoFix})
	insertRound(t, in, review, 1, "initial", a, submitted, true, false)
	insertRound(t, in, review, 2, "auto_fix", b, head, true, true)
	insertRound(t, in, review, 3, "auto_fix", "", head, false, true)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Bugs) != 2 || rec.Counts.OriginalWorker != 1 || rec.Counts.Pipeline != 1 || rec.Counts.FixedBeforeShipping != 2 {
		t.Fatalf("counts = %+v bugs=%+v", rec.Counts, rec.Bugs)
	}
}

func TestSnapshot_DedupesRepeatFindings(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	review := insertStep(t, in, types.StepReview, types.StepStatusCompleted)
	f := findingsJSON(types.Finding{
		ID: "review-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 2,
		Description: "nil deref", Action: types.ActionAutoFix,
	})
	insertRound(t, in, review, 1, "initial", f, submitted, true, false)
	insertRound(t, in, review, 2, "auto_fix", f, head, false, true)
	insertRound(t, in, review, 3, "initial", f, head, false, false)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Bugs) != 1 {
		t.Fatalf("bugs = %d, want 1 after dedup: %+v", len(rec.Bugs), rec.Bugs)
	}
}

// A selected finding the fixer failed to remove is still open, and when the
// run ships anyway it escaped. It is never reported as fixed.
func TestSnapshot_UnfixedSelectedFindingStaysOpenAndEscapesWhenShipped(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	review := insertStep(t, in, types.StepReview, types.StepStatusCompleted)
	f := findingsJSON(types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 2, Description: "nil deref", Action: types.ActionAutoFix})
	insertRound(t, in, review, 1, "initial", f, submitted, true, false)
	insertRound(t, in, review, 2, "auto_fix", f, head, false, true)

	snap, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.FixedBeforeShipping != 0 || snap.Bugs[0].Outcome != types.BugOutcomeStillOpen {
		t.Fatalf("snapshot = %+v", snap.Bugs)
	}

	in.Run.Status = types.RunCompleted
	final, err := Reconcile(context.Background(), in.Input, snap)
	if err != nil {
		t.Fatal(err)
	}
	if final.Counts.Escaped != 1 || final.Bugs[0].Outcome != types.BugOutcomeEscaped {
		t.Fatalf("final = %+v counts=%+v", final.Bugs, final.Counts)
	}

	in.Run.Status = types.RunFailed
	failed, err := Reconcile(context.Background(), in.Input, snap)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Counts.Escaped != 0 || failed.Bugs[0].Outcome != types.BugOutcomeStillOpen {
		t.Fatalf("a failed run shipped nothing: %+v", failed.Bugs)
	}
}

func TestSnapshot_SkippedReviewIsNotCleanScore(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	insertStep(t, in, types.StepReview, types.StepStatusSkipped)
	insertStep(t, in, types.StepTest, types.StepStatusSkipped)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status == types.AttributionStatusComplete {
		t.Fatal("skipped review must not be a complete clean score")
	}
	if !containsGap(rec, "review") {
		t.Fatalf("gaps = %v", rec.EvidenceGaps)
	}
}

func TestSnapshot_MissingSubmittedSHAIsUnknownNotPreExisting(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	in.Run.SubmittedHeadSHA = nil
	seedReviewFinding(t, in, "handler.go", 2, "nil deref", types.ActionAutoFix, true, submitted)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status == types.AttributionStatusComplete {
		t.Fatal("missing submitted SHA must not be a complete score")
	}
	if rec.Counts.Unknown != 1 || rec.Bugs[0].Attribution != types.AttributionUnknown {
		t.Fatalf("attribution = %+v, want unknown", rec.Bugs)
	}
}

// Without a resolvable branch base the pipeline half is still provable, but
// worker-versus-pre-existing is not, and must not default to a positive
// pre_existing claim.
func TestSnapshot_UnresolvableBranchBaseIsUnknownNotPreExisting(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	gitCmd(t, dir, "branch", "-D", "main")
	in := fixtureInput(t, dir, submitted, head)
	review := insertStep(t, in, types.StepReview, types.StepStatusCompleted)
	insertRound(t, in, review, 1, "initial", findingsJSON(
		types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 2, Description: "nil deref", Action: types.ActionAutoFix},
		types.Finding{ID: "review-2", Severity: types.FindingSeverityError, File: "handler.go", Line: 3, Description: "pipeline panic", Action: types.ActionAutoFix},
	), head, false, false)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.Unknown != 1 || rec.Counts.Pipeline != 1 || rec.Counts.PreExisting != 0 {
		t.Fatalf("counts = %+v bugs=%+v gaps=%v", rec.Counts, rec.Bugs, rec.EvidenceGaps)
	}
	if !containsGap(rec, "branch base") {
		t.Fatalf("gaps = %v", rec.EvidenceGaps)
	}
}

func TestReconcile_KeepsSnapshotAndNotesLaterCommits(t *testing.T) {
	dir, _, submitted, snapshotHead := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, snapshotHead)
	seedReviewFinding(t, in, "handler.go", 2, "nil deref", types.ActionAutoFix, true, submitted)
	snap, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}

	lintHead := commitFile(t, dir, "handler.go", "package handler\nfunc worker() {}\nfunc pipelineFix() {}\nfunc lintFmt() {}\n", "lint format")
	in.HeadSHA = lintHead
	lint := insertStep(t, in, types.StepLint, types.StepStatusCompleted)
	insertRound(t, in, lint, 1, "initial", findingsJSON(types.Finding{
		ID: "lint-1", Severity: types.FindingSeverityWarning, File: "handler.go", Line: 4,
		Description: "gofmt", Action: types.ActionAutoFix, Category: types.FindingCategoryLint,
	}), lintHead, true, true)

	final, err := Reconcile(context.Background(), in.Input, snap)
	if err != nil {
		t.Fatal(err)
	}
	if final.Phase != types.AttributionPhaseFinal {
		t.Fatalf("phase = %s", final.Phase)
	}
	if !final.Reconciled {
		t.Fatal("expected reconciled=true")
	}
	if final.SnapshotHeadSHA != snapshotHead {
		t.Fatalf("snapshot head rewritten to %s", final.SnapshotHeadSHA)
	}
	if final.FinalHeadSHA != lintHead {
		t.Fatalf("final head = %s", final.FinalHeadSHA)
	}
	if final.Counts.OriginalWorker != 1 {
		t.Fatalf("lost snapshot bug: %+v", final.Counts)
	}
	if final.Counts.NonBugs == 0 {
		t.Fatal("lint finding should be recorded as non-bug")
	}
	if final.Status != types.AttributionStatusComplete || containsGap(final, "reconciled") {
		t.Fatalf("a post-snapshot commit is normal, not an evidence gap: status=%s gaps=%v", final.Status, final.EvidenceGaps)
	}
}

// The final record keeps the snapshot's attribution even when the worktree is
// no longer readable; only outcomes are re-read.
func TestReconcile_KeepsSnapshotAttributionWithoutWorktree(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	seedReviewFinding(t, in, "handler.go", 2, "nil deref", types.ActionAutoFix, true, submitted)
	snap, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	in.WorkDir = ""
	final, err := Reconcile(context.Background(), in.Input, snap)
	if err != nil {
		t.Fatal(err)
	}
	if final.Counts.OriginalWorker != 1 || final.Bugs[0].Confidence != types.AttributionConfidenceHigh {
		t.Fatalf("snapshot attribution lost: %+v", final.Bugs)
	}
}

func TestReconcile_NilSnapshotIsUnavailable(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	rec, err := Reconcile(context.Background(), in.Input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != types.AttributionStatusUnavailable {
		t.Fatalf("status = %s", rec.Status)
	}
}

// A red CI check is a bug only when the fix round published a code change;
// a red job the fixer declined to touch is not a confirmed bug.
func TestSnapshot_CICheckIsABugOnlyWithAPublishedRepair(t *testing.T) {
	ciFinding := findingsJSON(types.Finding{ID: "ci-1", Severity: types.FindingSeverityError, Action: types.ActionAutoFix, Category: types.FindingCategoryCICheck, Check: "test", CheckID: "github-check-run:111", Description: "CI check failing: test - https://ci.example/runs/1/job/1"})
	// The same red check re-observed after the repair is a new check run: a
	// fresh per-execution provider ID and details link. It is the same
	// check, still red.
	relinked := findingsJSON(types.Finding{ID: "ci-1", Severity: types.FindingSeverityError, Action: types.ActionAutoFix, Category: types.FindingCategoryCICheck, Check: "test", CheckID: "github-check-run:222", Description: "CI check failing: test - https://ci.example/runs/2/job/9"})
	for _, tc := range []struct {
		name          string
		repair        bool
		afterFindings string
		wantBugs      int
		wantOutcome   string
	}{
		{"repair_published_and_green", true, "", 1, types.BugOutcomeFixedBeforeShipping},
		{"repair_published_still_red", true, ciFinding, 1, types.BugOutcomeStillOpen},
		{"repair_published_still_red_with_a_new_details_link", true, relinked, 1, types.BugOutcomeStillOpen},
		{"no_code_change_needed", false, ciFinding, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
			in := fixtureInput(t, dir, submitted, head)
			ci := insertStep(t, in, types.StepCI, types.StepStatusCompleted)
			insertRound(t, in, ci, 1, "initial", ciFinding, "", true, false)
			insertRepairRound(t, in, ci, 2, tc.afterFindings, tc.repair)

			rec, err := Reconcile(context.Background(), in.Input, &types.AttributionRecord{SnapshotHeadSHA: head})
			if err != nil {
				t.Fatal(err)
			}
			if len(rec.Bugs) != tc.wantBugs {
				t.Fatalf("bugs = %+v non_bugs=%+v", rec.Bugs, rec.NonBugs)
			}
			if tc.wantBugs == 0 {
				if rec.Counts.NonBugs != 1 {
					t.Fatalf("red check without a repair should be a non-bug: %+v", rec.NonBugs)
				}
				return
			}
			if rec.Bugs[0].Attribution != types.AttributionUnknown || rec.Bugs[0].Outcome != tc.wantOutcome {
				t.Fatalf("bug = %+v", rec.Bugs[0])
			}
			wantFixed := 0
			if tc.wantOutcome == types.BugOutcomeFixedBeforeShipping {
				wantFixed = 1
			}
			if rec.Counts.NonBugs != 0 || rec.Counts.FixedBeforeShipping != wantFixed {
				t.Fatalf("counts = %+v non_bugs=%+v", rec.Counts, rec.NonBugs)
			}
		})
	}
}

// The typed fixes_run_id link is confirmed only once the repair shipped: at
// the end of a completed run whose review and test completed.
func TestBugFixLink_ConfirmedOnlyWhenTheRepairShipped(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	insertStep(t, in, types.StepReview, types.StepStatusCompleted)
	insertStep(t, in, types.StepTest, types.StepStatusCompleted)
	id := "origin-run"
	in.Run.FixesRunID = &id

	snap, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if snap.BugFix == nil || snap.BugFix.OriginatingRunID != id {
		t.Fatalf("bug_fix = %+v", snap.BugFix)
	}
	if snap.BugFix.Confirmed {
		t.Fatalf("a snapshot is taken mid-run; link must be unconfirmed: %+v", snap.BugFix)
	}

	in.Run.Status = types.RunFailed
	failed, err := Reconcile(context.Background(), in.Input, snap)
	if err != nil {
		t.Fatal(err)
	}
	if failed.BugFix == nil || failed.BugFix.Confirmed {
		t.Fatalf("failed run confirmed a repair that never shipped: %+v", failed.BugFix)
	}
	if !containsGap(failed, "did not ship") {
		t.Fatalf("gaps = %v", failed.EvidenceGaps)
	}

	in.Run.Status = types.RunCompleted
	final, err := Reconcile(context.Background(), in.Input, snap)
	if err != nil {
		t.Fatal(err)
	}
	if final.BugFix == nil || !final.BugFix.Confirmed {
		t.Fatalf("expected confirmed link: %+v", final.BugFix)
	}
}

func TestBugFixLink_UnconfirmedWhenReviewSkipped(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	insertStep(t, in, types.StepReview, types.StepStatusSkipped)
	insertStep(t, in, types.StepTest, types.StepStatusCompleted)
	id := "origin-run"
	in.Run.FixesRunID = &id
	in.Run.Status = types.RunCompleted

	rec, err := Reconcile(context.Background(), in.Input, &types.AttributionRecord{SnapshotHeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	if rec.BugFix == nil || rec.BugFix.Confirmed {
		t.Fatalf("unconfirmed bug-fix should not be confirmed: %+v", rec.BugFix)
	}
}

// A squash leaves no commit that stands in for the submitted head, so a
// finding located at the rewritten head is unknown rather than a
// high-confidence worker claim from a stale map.
func TestSnapshot_RewrittenHistoryIsUnknown(t *testing.T) {
	dir, _, submitted, _ := repoWithWorkerBugAndPipelineFix(t)
	gitCmd(t, dir, "checkout", "--orphan", "squashed")
	gitCmd(t, dir, "commit", "-am", "squashed history")
	squashed := gitCmd(t, dir, "rev-parse", "HEAD")
	in := fixtureInput(t, dir, submitted, squashed)
	test := insertStep(t, in, types.StepTest, types.StepStatusCompleted)
	insertRound(t, in, test, 1, "initial", findingsJSON(types.Finding{ID: "test-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 2, Description: "nil deref", Action: types.ActionAutoFix}), "", true, false)
	insertRound(t, in, test, 2, "auto_fix", "", "", false, true)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if !containsGap(rec, "not an ancestor") {
		t.Fatalf("gaps = %v", rec.EvidenceGaps)
	}
	if rec.Status == types.AttributionStatusComplete {
		t.Fatal("ambiguous rewrite must not be a complete score")
	}
	if rec.Counts.Unknown != 1 || rec.Bugs[0].Attribution != types.AttributionUnknown {
		t.Fatalf("rewritten history attributed with confidence: %+v", rec.Bugs)
	}
}

// When the rebase step rewrote the branch, the head the first review round
// examined is the rebased worker submission; findings are attributed against
// it at medium confidence instead of the pre-rebase map.
func TestSnapshot_RebasedBranchUsesFirstReviewedHeadAsWorkerAnchor(t *testing.T) {
	dir, _, submitted, _ := repoWithWorkerBugAndPipelineFix(t)
	gitCmd(t, dir, "checkout", "main")
	commitFile(t, dir, "upstream.go", "package base\nfunc upstream() {}\n", "main moved")
	gitCmd(t, dir, "checkout", "feature")
	gitCmd(t, dir, "reset", "--hard", submitted)
	gitCmd(t, dir, "rebase", "main")
	rebased := gitCmd(t, dir, "rev-parse", "HEAD")
	head := commitFile(t, dir, "handler.go", "package handler\nfunc worker() { fixed() }\n", "pipeline in-place fix")
	in := fixtureInput(t, dir, submitted, head)
	review := insertStep(t, in, types.StepReview, types.StepStatusCompleted)
	insertRound(t, in, review, 1, "initial", findingsJSON(types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 2, Description: "nil deref", Action: types.ActionAutoFix}), rebased, true, false)
	insertRound(t, in, review, 2, "auto_fix", findingsJSON(types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 2, Description: "fixed() ignores its error", Action: types.ActionAutoFix}), head, false, true)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.OriginalWorker != 1 || rec.Counts.Pipeline != 1 {
		t.Fatalf("counts = %+v bugs=%+v gaps=%v", rec.Counts, rec.Bugs, rec.EvidenceGaps)
	}
	for _, bug := range rec.Bugs {
		if bug.Confidence != types.AttributionConfidenceMedium {
			t.Fatalf("post-rebase anchor must not claim high confidence: %+v", bug)
		}
	}
	if !containsGap(rec, "rewritten before review") {
		t.Fatalf("gaps = %v", rec.EvidenceGaps)
	}
}

// A review that restarted after a mid-run rewrite (a CI merge-conflict repair
// rebased the branch) examined a head the submission is no longer an ancestor
// of. Only the head the first review round examined may stand in for the
// submission; a later round's head would count the pipeline's own commits as
// the worker's.
func TestSnapshot_MidRunRewriteHasNoWorkerStandIn(t *testing.T) {
	dir, _, submitted, pipelineHead := repoWithWorkerBugAndPipelineFix(t)
	gitCmd(t, dir, "checkout", "main")
	commitFile(t, dir, "upstream.go", "package base\nfunc upstream() {}\n", "main moved")
	gitCmd(t, dir, "checkout", "feature")
	gitCmd(t, dir, "rebase", "main")
	rebased := gitCmd(t, dir, "rev-parse", "HEAD")
	head := commitFile(t, dir, "handler.go", "package handler\nfunc worker() {}\nfunc pipelineFix() { fixed() }\n", "second repair")
	in := fixtureInput(t, dir, submitted, head)
	review := insertStep(t, in, types.StepReview, types.StepStatusCompleted)
	insertRound(t, in, review, 1, "initial", findingsJSON(types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 2, Description: "nil deref", Action: types.ActionAutoFix}), submitted, true, false)
	insertRound(t, in, review, 2, "auto_fix", "", pipelineHead, false, true)
	insertRound(t, in, review, 3, "initial", findingsJSON(types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 3, Description: "pipelineFix panics", Action: types.ActionAutoFix}), rebased, true, false)
	insertRound(t, in, review, 4, "auto_fix", "", head, false, true)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	byDesc := bugsByDescription(rec)
	if got := byDesc["nil deref"]; got.Attribution != types.AttributionOriginalWorker || got.Confidence != types.AttributionConfidenceHigh {
		t.Fatalf("worker bug located before the rewrite = %+v", got)
	}
	if got := byDesc["pipelineFix panics"]; got.Attribution != types.AttributionUnknown {
		t.Fatalf("pipeline line after the rewrite was attributed: %+v gaps=%v", got, rec.EvidenceGaps)
	}
	if !containsGap(rec, "not an ancestor") || containsGap(rec, "rewritten before review") {
		t.Fatalf("gaps = %v", rec.EvidenceGaps)
	}
	if rec.Status == types.AttributionStatusComplete {
		t.Fatal("a mid-run rewrite must not be a complete score")
	}
}

// A finding in a file the located commit does not hold (the Test step's own
// uncommitted test file, a path the agent misreported) is not in any diff,
// which is not evidence that the code predates the branch.
func TestSnapshot_FindingInAFileAbsentAtItsCommitIsUnknown(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	write(t, dir, "handler_test.go", "package handler\nfunc TestWorker() {\n\tworker()\n}\n")
	in := fixtureInput(t, dir, submitted, head)
	test := insertStep(t, in, types.StepTest, types.StepStatusCompleted)
	insertRound(t, in, test, 1, "initial", testedFindingsJSON(head, types.Finding{ID: "test-1", Severity: types.FindingSeverityError, File: "handler_test.go", Line: 3, Description: "test never asserts", Action: types.ActionAutoFix}), "", true, false)
	insertRound(t, in, test, 2, "auto_fix", "", "", false, true)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.PreExisting != 0 || rec.Counts.Unknown != 1 || rec.Bugs[0].Attribution != types.AttributionUnknown {
		t.Fatalf("absent file was attributed: %+v %+v", rec.Counts, rec.Bugs)
	}
	if !strings.Contains(strings.Join(rec.Bugs[0].Evidence, "\n"), "not present at") {
		t.Fatalf("evidence = %v", rec.Bugs[0].Evidence)
	}
}

// A line past the end of the file at the located commit names nothing.
func TestSnapshot_FindingBeyondTheEndOfItsFileIsUnknown(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	seedReviewFinding(t, in, "base.go", 40, "pre() overflows", types.ActionAutoFix, true, submitted)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.PreExisting != 0 || rec.Counts.Unknown != 1 || rec.Bugs[0].Attribution != types.AttributionUnknown {
		t.Fatalf("out-of-range line was attributed: %+v %+v", rec.Counts, rec.Bugs)
	}
	if !strings.Contains(strings.Join(rec.Bugs[0].Evidence, "\n"), "beyond the end of the file") {
		t.Fatalf("evidence = %v", rec.Bugs[0].Evidence)
	}
}

// A rerun submits the gate head, which may already carry an earlier run's
// pipeline commits; the lines it brought cannot be split between the worker
// and that pipeline. This run's own pipeline lines and pre-existing code are
// still provable.
func TestSnapshot_RerunGateHeadLinesAreUnknownNotWorker(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	in.Run.Rerun = true
	review := insertStep(t, in, types.StepReview, types.StepStatusCompleted)
	insertRound(t, in, review, 1, "initial", findingsJSON(
		types.Finding{ID: "review-1", Severity: types.FindingSeverityError, File: "handler.go", Line: 2, Description: "nil deref", Action: types.ActionAutoFix},
		types.Finding{ID: "review-2", Severity: types.FindingSeverityError, File: "handler.go", Line: 3, Description: "pipeline panic", Action: types.ActionAutoFix},
		types.Finding{ID: "review-3", Severity: types.FindingSeverityError, File: "base.go", Line: 2, Description: "pre-existing off-by-one", Action: types.ActionAutoFix},
	), head, false, false)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.OriginalWorker != 0 || rec.Counts.Unknown != 1 || rec.Counts.Pipeline != 1 || rec.Counts.PreExisting != 1 {
		t.Fatalf("counts = %+v bugs=%+v", rec.Counts, rec.Bugs)
	}
	byDesc := bugsByDescription(rec)
	if got := byDesc["nil deref"]; got.Attribution != types.AttributionUnknown || got.Confidence != types.AttributionConfidenceUnknown {
		t.Fatalf("gate-head line was claimed for the worker: %+v", got)
	}
	if !containsGap(rec, "rerun submitted the gate head") {
		t.Fatalf("gaps = %v", rec.EvidenceGaps)
	}
	if rec.Status == types.AttributionStatusComplete {
		t.Fatal("a rerun cannot be a complete worker score")
	}
}

func TestSkippedRecordIsNotClean(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	rec := SkippedRecord(in.Input)
	if rec.Status != types.AttributionStatusSkipped || rec.Phase != types.AttributionPhaseFinal {
		t.Fatalf("status = %s phase = %s", rec.Status, rec.Phase)
	}
	if rec.Counts.OriginalWorker != 0 {
		t.Fatalf("skipped record invented bugs: %+v", rec.Counts)
	}
}

func TestWorkerProvenanceUnknownWhenAbsent(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Worker.Tool != types.WorkerFieldUnknown || rec.Worker.Model != types.WorkerFieldUnknown {
		t.Fatalf("invented worker identity: %+v", rec.Worker)
	}
}

func TestWorkerProvenancePreservedWhenSupplied(t *testing.T) {
	dir, _, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, submitted, head)
	raw := `{"task_id":"task-1","tool":"grok","model":"grok-4.6","provider":"xai"}`
	in.Run.WorkerProvenanceJSON = &raw
	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Worker.Tool != "grok" || rec.TaskID != "task-1" {
		t.Fatalf("worker = %+v task=%s", rec.Worker, rec.TaskID)
	}
}

func repoWithWorkerBugAndPipelineFix(t *testing.T) (dir, base, submitted, pipelineHead string) {
	t.Helper()
	dir = t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	write(t, dir, "base.go", "package base\nfunc pre() int { return 1 }\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	base = gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "-b", "feature")
	write(t, dir, "handler.go", "package handler\nfunc worker() {}\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "worker change")
	submitted = gitCmd(t, dir, "rev-parse", "HEAD")

	write(t, dir, "handler.go", "package handler\nfunc worker() {}\nfunc pipelineFix() {}\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "pipeline fix")
	pipelineHead = gitCmd(t, dir, "rev-parse", "HEAD")
	return dir, base, submitted, pipelineHead
}

type testEnv struct {
	Input
	DB *db.DB
}

// fixtureInput records the run the way the daemon does on a first push: the
// zero SHA as BaseSHA, never the branch base.
func fixtureInput(t *testing.T, dir, submitted, head string) *testEnv {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(dir, "https://example.com/test/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", submitted, zeroSHA)
	if err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	run.SubmittedHeadSHA = &submitted
	return &testEnv{
		Input: Input{
			WorkDir: dir,
			Run:     run,
			Repo:    repo,
			Rounds:  map[string][]*db.StepRound{},
			HeadSHA: head,
		},
		DB: database,
	}
}

func insertStep(t *testing.T, in *testEnv, name types.StepName, status types.StepStatus) *db.StepResult {
	t.Helper()
	step, err := in.DB.InsertStepResult(in.Run.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.DB.UpdateStepStatus(step.ID, status); err != nil {
		t.Fatal(err)
	}
	step.Status = status
	in.Steps = append(in.Steps, step)
	return step
}

func seedReviewFinding(t *testing.T, in *testEnv, file string, line int, desc, action string, fixed bool, locate string) {
	t.Helper()
	review := insertStep(t, in, types.StepReview, types.StepStatusCompleted)
	f := findingsJSON(types.Finding{
		ID: "review-1", Severity: types.FindingSeverityError, File: file, Line: line,
		Description: desc, Action: action,
	})
	insertRound(t, in, review, 1, "initial", f, locate, fixed, false)
	if fixed {
		insertRound(t, in, review, 2, "auto_fix", "", locate, false, true)
	}
}

func insertRound(t *testing.T, in *testEnv, step *db.StepResult, n int, trigger, findings, reviewed string, selected, isFix bool) *db.StepRound {
	t.Helper()
	var raw *string
	if findings != "" {
		raw = &findings
	}
	var fixSummary *string
	if isFix {
		s := "fix applied"
		fixSummary = &s
	}
	round, err := in.DB.InsertReviewStepRound(step.ID, n, trigger, raw, fixSummary, reviewed, 1)
	if err != nil {
		t.Fatal(err)
	}
	if selected {
		ids := `["review-1"]`
		if findings != "" {
			parsed, err := types.ParseFindingsJSON(findings)
			if err != nil {
				t.Fatal(err)
			}
			ids = `["` + parsed.Items[0].ID + `"]`
		}
		if err := in.DB.SetStepRoundSelection(round.ID, &ids, db.RoundSelectionSourceAutoFix); err != nil {
			t.Fatal(err)
		}
		round.SelectedFindingIDs = &ids
	}
	in.Rounds[step.ID] = append(in.Rounds[step.ID], round)
	return round
}

func insertRepairRound(t *testing.T, in *testEnv, step *db.StepResult, n int, findings string, repairPublished bool) {
	t.Helper()
	var raw *string
	if findings != "" {
		raw = &findings
	}
	summary := "repair"
	round, err := in.DB.InsertStepRoundWithRepair(step.ID, n, "auto_fix", raw, &summary, repairPublished, 1)
	if err != nil {
		t.Fatal(err)
	}
	in.Rounds[step.ID] = append(in.Rounds[step.ID], round)
}

func findingsJSON(items ...types.Finding) string {
	return testedFindingsJSON("", items...)
}

// testedFindingsJSON is the Test step's shape: the head the round validated
// travels inside the findings as tested_head_sha.
func testedFindingsJSON(testedHead string, items ...types.Finding) string {
	raw, err := types.MarshalFindingsJSON(types.Findings{Items: items, Summary: "test", RiskLevel: "low", TestedHeadSHA: testedHead})
	if err != nil {
		panic(err)
	}
	return raw
}

func bugsByDescription(rec *types.AttributionRecord) map[string]types.AttributedBug {
	out := make(map[string]types.AttributedBug, len(rec.Bugs))
	for _, bug := range rec.Bugs {
		out[bug.Description] = bug
	}
	return out
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitFile(t *testing.T, dir, name, content, msg string) string {
	t.Helper()
	write(t, dir, name, content)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", msg)
	return gitCmd(t, dir, "rev-parse", "HEAD")
}

func containsGap(rec *types.AttributionRecord, needle string) bool {
	for _, g := range rec.EvidenceGaps {
		if strings.Contains(g, needle) {
			return true
		}
	}
	return false
}

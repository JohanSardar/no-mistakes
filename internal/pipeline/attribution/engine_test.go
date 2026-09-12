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

func TestSnapshot_OriginalWorkerFix(t *testing.T) {
	dir, base, submitted, pipelineHead := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, pipelineHead)
	seedReviewFinding(t, in, "handler.go", 2, "nil deref", types.ActionAutoFix, true, submitted)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status == types.AttributionStatusUnavailable {
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
	if rec.Counts.OriginalWorker != 1 {
		t.Fatalf("counts = %+v bugs=%+v gaps=%v", rec.Counts, rec.Bugs, rec.EvidenceGaps)
	}
	if rec.Bugs[0].Attribution != types.AttributionOriginalWorker {
		t.Fatalf("attribution = %s evidence=%v", rec.Bugs[0].Attribution, rec.Bugs[0].Evidence)
	}
	if rec.Bugs[0].Outcome != types.BugOutcomeFixedBeforeShipping {
		t.Fatalf("outcome = %s", rec.Bugs[0].Outcome)
	}
}

func TestSnapshot_PipelineIntroducedFix(t *testing.T) {
	dir, base, submitted, pipelineHead := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, pipelineHead)
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

func TestSnapshot_PreExistingNotWorker(t *testing.T) {
	dir, base, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, head)
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

func TestSnapshot_UnknownWithoutFileLine(t *testing.T) {
	dir, base, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, head)
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
	dir, base, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, head)
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
	dir, base, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, head)
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

func TestSnapshot_DiffAloneIsNotABug(t *testing.T) {
	dir, base, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, head)
	insertStep(t, in, types.StepReview, types.StepStatusCompleted)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Counts.OriginalWorker != 0 || len(rec.Bugs) != 0 {
		t.Fatalf("diff was treated as a bug: %+v", rec.Bugs)
	}
}

func TestSnapshot_DedupesRepeatFindings(t *testing.T) {
	dir, base, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, head)
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

func TestSnapshot_SkippedReviewIsNotCleanScore(t *testing.T) {
	dir, base, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, head)
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

func TestSnapshot_MissingSubmittedSHAUnknown(t *testing.T) {
	dir, base, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, head)
	in.Run.SubmittedHeadSHA = nil
	seedReviewFinding(t, in, "handler.go", 2, "nil deref", types.ActionAutoFix, true, submitted)

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status == types.AttributionStatusComplete {
		t.Fatal("missing submitted SHA must not be a complete score")
	}
	if rec.Counts.Unknown != 1 && rec.Counts.OriginalWorker != 0 {
		// Without submitted, worker vs pipeline cannot be proven.
		if rec.Bugs[0].Attribution != types.AttributionUnknown && rec.Bugs[0].Attribution != types.AttributionPreExisting {
			t.Fatalf("attribution = %s, want unknown-ish", rec.Bugs[0].Attribution)
		}
	}
}

func TestReconcile_KeepsSnapshotAndNotesLaterCommits(t *testing.T) {
	dir, base, submitted, snapshotHead := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, snapshotHead)
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
}

func TestReconcile_NilSnapshotIsUnavailable(t *testing.T) {
	dir, base, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, head)
	rec, err := Reconcile(context.Background(), in.Input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != types.AttributionStatusUnavailable {
		t.Fatalf("status = %s", rec.Status)
	}
}

func TestSnapshot_BugFixRunLinkRequiresTypedSignal(t *testing.T) {
	dir, base, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, head)
	insertStep(t, in, types.StepReview, types.StepStatusCompleted)
	insertStep(t, in, types.StepTest, types.StepStatusCompleted)
	id := "origin-run"
	in.Run.FixesRunID = &id

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.BugFix == nil || rec.BugFix.OriginatingRunID != id {
		t.Fatalf("bug_fix = %+v", rec.BugFix)
	}
	if !rec.BugFix.Confirmed {
		t.Fatalf("expected confirmed link, evidence=%v", rec.BugFix.Evidence)
	}
}

func TestSnapshot_BugFixRunUnconfirmedWhenReviewSkipped(t *testing.T) {
	dir, base, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, head)
	insertStep(t, in, types.StepReview, types.StepStatusSkipped)
	insertStep(t, in, types.StepTest, types.StepStatusCompleted)
	id := "origin-run"
	in.Run.FixesRunID = &id

	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.BugFix == nil || rec.BugFix.Confirmed {
		t.Fatalf("unconfirmed bug-fix should not be confirmed: %+v", rec.BugFix)
	}
}

func TestSnapshot_RewrittenHistoryIsUnknown(t *testing.T) {
	dir, base, submitted, _ := repoWithWorkerBugAndPipelineFix(t)
	// Squash: new root-like commit that does not have submitted as ancestor.
	gitCmd(t, dir, "checkout", "--orphan", "squashed")
	gitCmd(t, dir, "commit", "-am", "squashed history")
	squashed := gitCmd(t, dir, "rev-parse", "HEAD")
	in := fixtureInput(t, dir, base, submitted, squashed)
	seedReviewFinding(t, in, "handler.go", 2, "nil deref", types.ActionAutoFix, true, submitted)

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
}

func TestSkippedRecordIsNotClean(t *testing.T) {
	dir, base, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, head)
	rec := SkippedRecord(in.Input)
	if rec.Status != types.AttributionStatusSkipped {
		t.Fatalf("status = %s", rec.Status)
	}
	if rec.Counts.OriginalWorker != 0 {
		t.Fatalf("skipped record invented bugs: %+v", rec.Counts)
	}
}

func TestWorkerProvenanceUnknownWhenAbsent(t *testing.T) {
	dir, base, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, head)
	rec, err := Snapshot(context.Background(), in.Input)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Worker.Tool != types.WorkerFieldUnknown || rec.Worker.Model != types.WorkerFieldUnknown {
		t.Fatalf("invented worker identity: %+v", rec.Worker)
	}
}

func TestWorkerProvenancePreservedWhenSupplied(t *testing.T) {
	dir, base, submitted, head := repoWithWorkerBugAndPipelineFix(t)
	in := fixtureInput(t, dir, base, submitted, head)
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

func fixtureInput(t *testing.T, dir, base, submitted, head string) *testEnv {
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
	run, err := database.InsertRun(repo.ID, "feature", submitted, base)
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

func insertRound(t *testing.T, in *testEnv, step *db.StepResult, n int, trigger, findings, starting string, selected, isFix bool) {
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
	round, err := in.DB.InsertReviewStepRoundWithProvenance(step.ID, n, trigger, raw, fixSummary, starting, starting, "", nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if selected {
		ids := `["review-1"]`
		if err := in.DB.SetStepRoundSelection(round.ID, &ids, db.RoundSelectionSourceAutoFix); err != nil {
			t.Fatal(err)
		}
		round.SelectedFindingIDs = &ids
	}
	in.Rounds[step.ID] = append(in.Rounds[step.ID], round)
}

func findingsJSON(items ...types.Finding) string {
	raw, err := types.MarshalFindingsJSON(types.Findings{Items: items, Summary: "test", RiskLevel: "low"})
	if err != nil {
		panic(err)
	}
	return raw
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

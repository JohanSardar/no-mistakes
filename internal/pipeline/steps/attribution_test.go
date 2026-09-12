package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/attribution"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAllSteps_PlacesAttributionAfterDocumentBeforeLint(t *testing.T) {
	names := stepNames(AllSteps())
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "document,attribution,lint") {
		t.Fatalf("core sequence = %s, want document,attribution,lint", joined)
	}
	if strings.Index(joined, "attribution") < strings.Index(joined, "document") {
		t.Fatal("attribution ran before document")
	}
	if strings.Index(joined, "lint") < strings.Index(joined, "attribution") {
		t.Fatal("lint ran before attribution")
	}
}

func TestAttributionStep_DoesNotParkOrRestart(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "mock"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.SubmittedHeadSHA = &headSHA

	review, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"error","file":"feature.txt","line":1,"description":"bug","action":"auto-fix"}],"summary":"1","risk_level":"low"}`
	round, err := sctx.DB.InsertReviewStepRoundWithProvenance(review.ID, 1, "initial", &findings, nil, headSHA, headSHA, "", nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["review-1"]`
	if err := sctx.DB.SetStepRoundSelection(round.ID, &selected, db.RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}
	fixSummary := "fixed"
	if _, err := sctx.DB.InsertStepRound(review.ID, 2, "auto_fix", nil, &fixSummary, 1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature code\nfixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "pipeline fix")
	sctx.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")

	outcome, err := (&AttributionStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.AutoFixable || outcome.RestartFrom != "" {
		t.Fatalf("attribution must not park or restart: %+v", outcome)
	}
	stored, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AttributionSnapshotJSON == nil || sctx.Run.AttributionSnapshotJSON == nil {
		t.Fatal("snapshot was not persisted on the run")
	}
	rec, err := attribution.UnmarshalRecord(*stored.AttributionSnapshotJSON)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Phase != types.AttributionPhaseSnapshot {
		t.Fatalf("phase = %s", rec.Phase)
	}
	if rec.Counts.OriginalWorker != 1 || rec.Counts.FixedBeforeShipping != 1 {
		t.Fatalf("counts = %+v bugs=%+v gaps=%v", rec.Counts, rec.Bugs, rec.EvidenceGaps)
	}
	if rec.Bugs[0].Attribution != types.AttributionOriginalWorker {
		t.Fatalf("attribution = %+v", rec.Bugs[0])
	}
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 0 || !strings.Contains(parsed.Summary, "attribution snapshot") {
		t.Fatalf("step findings should carry only the summary line: %s", outcome.Findings)
	}
	if strings.Contains(outcome.Findings, `"bugs"`) {
		t.Fatalf("step findings duplicated the attribution record: %s", outcome.Findings)
	}
}

// The Test step's verdict roll-up rides beside the scenario findings with the
// same severity and action and is selected with them, but it summarises the
// defect rather than being one; attribution records it as a non-bug.
func TestAttributionStep_VerdictRollupIsNotABug(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "mock"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.SubmittedHeadSHA = &headSHA

	test, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	round1 := types.Findings{
		Items:         []types.Finding{{Severity: types.FindingSeverityError, File: "feature.txt", Line: 1, Description: "login scenario fails", Action: types.ActionAutoFix}},
		Scenarios:     []types.TestScenario{{Name: "login", Result: types.ScenarioResultFail, Live: true}},
		Verdict:       types.TestVerdictNoGo,
		TestedHeadSHA: headSHA,
		Summary:       "no-go",
		RiskLevel:     "low",
	}
	round1.Items = append(round1.Items, verdictFindings(round1)...)
	round1 = types.NormalizeFindings(round1, string(types.StepTest))
	if len(round1.Items) != 2 {
		t.Fatalf("expected the scenario finding plus the verdict roll-up, got %+v", round1.Items)
	}
	raw1, err := types.MarshalFindingsJSON(round1)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.InsertStepRound(test.ID, 1, "initial", &raw1, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["` + round1.Items[0].ID + `","` + round1.Items[1].ID + `"]`
	if err := sctx.DB.SetStepRoundSelection(round.ID, &selected, db.RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature code fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "pipeline fix")
	sctx.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	clean, err := types.MarshalFindingsJSON(types.Findings{Verdict: types.TestVerdictGo, TestedHeadSHA: sctx.Run.HeadSHA, Summary: "go", RiskLevel: "low"})
	if err != nil {
		t.Fatal(err)
	}
	fixSummary := "fixed"
	if _, err := sctx.DB.InsertStepRound(test.ID, 2, "auto_fix", &clean, &fixSummary, 1); err != nil {
		t.Fatal(err)
	}

	if _, err := (&AttributionStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	rec, err := attribution.UnmarshalRecord(*sctx.Run.AttributionSnapshotJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Bugs) != 1 || rec.Counts.OriginalWorker != 1 || rec.Counts.Unknown != 0 || rec.Counts.FixedBeforeShipping != 1 {
		t.Fatalf("verdict roll-up counted as a bug: bugs=%+v counts=%+v", rec.Bugs, rec.Counts)
	}
	if rec.Counts.NonBugs != 1 || rec.NonBugs[0].Kind != types.ChangeKindOther || !strings.Contains(rec.NonBugs[0].Description, "live validation verdict") {
		t.Fatalf("verdict roll-up should be recorded as a non-bug: %+v", rec.NonBugs)
	}
}

func TestAttributionStep_MissingMetadataIsNotCleanScore(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "mock"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.SubmittedHeadSHA = nil
	review, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(review.ID, types.StepStatusSkipped); err != nil {
		t.Fatal(err)
	}

	outcome, err := (&AttributionStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if sctx.Run.AttributionSnapshotJSON == nil {
		t.Fatal("snapshot was not persisted")
	}
	rec, err := attribution.UnmarshalRecord(*sctx.Run.AttributionSnapshotJSON)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status == types.AttributionStatusComplete {
		t.Fatalf("missing metadata scored complete: %+v", rec)
	}
	if !strings.Contains(outcome.Findings, "attribution snapshot "+rec.Status) {
		t.Fatalf("summary does not report the %s status: %s", rec.Status, outcome.Findings)
	}
}

var _ pipeline.Step = (*AttributionStep)(nil)

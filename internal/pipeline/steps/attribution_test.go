package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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
	if sctx.Run.AttributionSnapshotJSON == nil {
		t.Fatal("snapshot was not persisted")
	}
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Attribution == nil {
		t.Fatal("findings missing attribution record")
	}
	if parsed.Attribution.Phase != types.AttributionPhaseSnapshot {
		t.Fatalf("phase = %s", parsed.Attribution.Phase)
	}
	if parsed.Attribution.Counts.OriginalWorker != 1 || parsed.Attribution.Counts.FixedBeforeShipping != 1 {
		t.Fatalf("counts = %+v bugs=%+v gaps=%v", parsed.Attribution.Counts, parsed.Attribution.Bugs, parsed.Attribution.EvidenceGaps)
	}
	if parsed.Attribution.Bugs[0].Attribution != types.AttributionOriginalWorker {
		t.Fatalf("attribution = %+v", parsed.Attribution.Bugs[0])
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
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Attribution == nil || parsed.Attribution.Status == types.AttributionStatusComplete {
		t.Fatalf("missing metadata scored complete: %+v", parsed.Attribution)
	}
}

var _ pipeline.Step = (*AttributionStep)(nil)

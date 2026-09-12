package pipeline

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/attribution"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The run's terminal transition (completeRun/failRun) reconciles the pre-Lint
// attribution snapshot into runs.attribution_json and replaces the attribution
// step's findings with the final record, for a completed and a failed run.
func TestExecutor_TerminalRunReconcilesAttribution(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lintErr    error
		wantStatus types.RunStatus
	}{
		{"completed", nil, types.RunCompleted},
		{"failed", errors.New("lint exploded"), types.RunFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, _, repo := setupTest(t)
			workDir := t.TempDir()
			initGitRepo(t, workDir)
			execGit(t, workDir, "branch", "-M", "main")
			execGit(t, workDir, "checkout", "-b", "feature")
			writeTestFile(t, workDir, "feature.txt", "feature code\n")
			execGit(t, workDir, "add", ".")
			execGit(t, workDir, "commit", "-m", "worker change")
			headSHA := gitHead(t, workDir)
			// The daemon records the gate's previous head as BaseSHA: the zero
			// SHA for a brand-new branch, never the branch base.
			run, err := database.InsertRun(repo.ID, "feature", headSHA, "0000000000000000000000000000000000000000")
			if err != nil {
				t.Fatal(err)
			}

			reviewCalls := 0
			review := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				reviewCalls++
				if reviewCalls == 1 {
					return &StepOutcome{
						NeedsApproval:         true,
						AutoFixable:           true,
						ReviewApprovedHeadSHA: headSHA,
						Findings:              `{"findings":[{"severity":"error","file":"feature.txt","line":1,"description":"nil deref","action":"auto-fix"}],"summary":"1 issue"}`,
					}, nil
				}
				return &StepOutcome{ReviewApprovedHeadSHA: headSHA, FixSummary: "fixed"}, nil
			}}
			attributionStep := &adaptiveCallStep{name: types.StepAttribution, fn: func(sctx *StepContext) (*StepOutcome, error) {
				steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
				if err != nil {
					return nil, err
				}
				in := attribution.Input{WorkDir: sctx.WorkDir, Run: sctx.Run, Repo: sctx.Repo, Steps: steps, Rounds: map[string][]*db.StepRound{}, HeadSHA: sctx.Run.HeadSHA}
				for _, step := range steps {
					rounds, err := sctx.DB.GetRoundsByStep(step.ID)
					if err != nil {
						return nil, err
					}
					in.Rounds[step.ID] = rounds
				}
				rec, err := attribution.Snapshot(sctx.Ctx, in)
				if err != nil {
					return nil, err
				}
				raw, err := attribution.MarshalRecord(rec)
				if err != nil {
					return nil, err
				}
				if err := sctx.DB.SetRunAttributionSnapshot(sctx.Run.ID, raw); err != nil {
					return nil, err
				}
				sctx.Run.AttributionSnapshotJSON = &raw
				encoded, err := types.MarshalFindingsJSON(attribution.FindingsFrom(rec))
				if err != nil {
					return nil, err
				}
				return &StepOutcome{Findings: encoded}, nil
			}}
			lint := &mockStep{name: types.StepLint, outcome: &StepOutcome{}, err: tc.lintErr}

			exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Review: 1}}, nil, []Step{review, attributionStep, lint}, nil)
			err = exec.Execute(context.Background(), run, repo, workDir)
			if (err != nil) != (tc.lintErr != nil) {
				t.Fatalf("Execute() error = %v", err)
			}

			stored, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != tc.wantStatus {
				t.Fatalf("run status = %s, want %s", stored.Status, tc.wantStatus)
			}
			if stored.AttributionSnapshotJSON == nil || stored.AttributionJSON == nil {
				t.Fatalf("snapshot/final not persisted: snapshot=%v final=%v", stored.AttributionSnapshotJSON != nil, stored.AttributionJSON != nil)
			}
			snapshot, err := attribution.UnmarshalRecord(*stored.AttributionSnapshotJSON)
			if err != nil {
				t.Fatal(err)
			}
			final, err := attribution.UnmarshalRecord(*stored.AttributionJSON)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Phase != types.AttributionPhaseSnapshot || final.Phase != types.AttributionPhaseFinal {
				t.Fatalf("phases = %s / %s", snapshot.Phase, final.Phase)
			}
			if final.SnapshotHeadSHA != snapshot.SnapshotHeadSHA || final.FinalHeadSHA != headSHA {
				t.Fatalf("final heads = %s / %s, snapshot head %s", final.SnapshotHeadSHA, final.FinalHeadSHA, snapshot.SnapshotHeadSHA)
			}
			if final.Counts.OriginalWorker != 1 || final.Counts.FixedBeforeShipping != 1 || final.Counts.Escaped != 0 {
				t.Fatalf("final counts = %+v bugs=%+v gaps=%v", final.Counts, final.Bugs, final.EvidenceGaps)
			}
			if final.Bugs[0].Attribution != types.AttributionOriginalWorker || final.Bugs[0].Confidence != types.AttributionConfidenceHigh {
				t.Fatalf("final bug = %+v", final.Bugs[0])
			}

			steps, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			var attrFindings string
			for _, step := range steps {
				if step.StepName == types.StepAttribution && step.FindingsJSON != nil {
					attrFindings = *step.FindingsJSON
				}
			}
			parsed, err := types.ParseFindingsJSON(attrFindings)
			if err != nil {
				t.Fatalf("attribution step findings: %v (%q)", err, attrFindings)
			}
			if parsed.Attribution == nil || parsed.Attribution.Phase != types.AttributionPhaseFinal {
				t.Fatalf("attribution step findings were not replaced by the final record: %+v", parsed.Attribution)
			}
			if !strings.Contains(parsed.Summary, "attribution final") {
				t.Fatalf("summary = %q", parsed.Summary)
			}
		})
	}
}

func gitHead(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

package steps

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/attribution"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// AttributionStep records observational bug attribution after Documentation
// and before Lint. It never parks, never auto-fixes, and never restarts the
// pipeline: review fixes are already re-reviewed.
type AttributionStep struct{}

func (s *AttributionStep) Name() types.StepName { return types.StepAttribution }

func (s *AttributionStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	in, err := attributionInput(sctx)
	if err != nil {
		return nil, err
	}
	rec, err := attribution.Snapshot(sctx.Ctx, in)
	if err != nil {
		return nil, fmt.Errorf("attribution snapshot: %w", err)
	}
	if err := persistAttribution(sctx, rec, true); err != nil {
		return nil, err
	}
	sctx.Log(fmt.Sprintf("attribution snapshot %s: worker=%d pipeline=%d pre_existing=%d unknown=%d\n",
		rec.Status, rec.Counts.OriginalWorker, rec.Counts.Pipeline, rec.Counts.PreExisting, rec.Counts.Unknown))
	encoded, err := types.MarshalFindingsJSON(attribution.FindingsFrom(rec))
	if err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{ExitCode: 0, Findings: encoded}, nil
}

func attributionInput(sctx *pipeline.StepContext) (attribution.Input, error) {
	in := attribution.Input{WorkDir: sctx.WorkDir, Run: sctx.Run, Repo: sctx.Repo, Rounds: map[string][]*db.StepRound{}}
	if sctx.Run != nil {
		in.HeadSHA = sctx.Run.HeadSHA
		if sctx.Run.PRURL != nil {
			in.PRURL = *sctx.Run.PRURL
		}
	}
	if sctx.DB == nil || sctx.Run == nil {
		return in, nil
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return in, fmt.Errorf("load steps for attribution: %w", err)
	}
	in.Steps = steps
	for _, step := range steps {
		rounds, err := sctx.DB.GetRoundsByStep(step.ID)
		if err != nil {
			return in, fmt.Errorf("load rounds for %s: %w", step.StepName, err)
		}
		in.Rounds[step.ID] = rounds
	}
	if sctx.WorkDir != "" {
		if live, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir); err == nil && live != "" {
			in.HeadSHA = live
		}
	}
	return in, nil
}

func persistAttribution(sctx *pipeline.StepContext, rec *types.AttributionRecord, snapshot bool) error {
	if sctx == nil || sctx.DB == nil || sctx.Run == nil || rec == nil {
		return nil
	}
	raw, err := attribution.MarshalRecord(rec)
	if err != nil {
		return err
	}
	if snapshot {
		if err := sctx.DB.SetRunAttributionSnapshot(sctx.Run.ID, raw); err != nil {
			return err
		}
		sctx.Run.AttributionSnapshotJSON = &raw
		return nil
	}
	if err := sctx.DB.SetRunAttributionFinal(sctx.Run.ID, raw); err != nil {
		return err
	}
	sctx.Run.AttributionJSON = &raw
	return nil
}

func reconcileAttribution(sctx *pipeline.StepContext) error {
	if sctx == nil || sctx.Run == nil {
		return nil
	}
	in, err := attributionInput(sctx)
	if err != nil {
		return err
	}
	var snapshot *types.AttributionRecord
	if sctx.Run.AttributionSnapshotJSON != nil {
		snapshot, err = attribution.UnmarshalRecord(*sctx.Run.AttributionSnapshotJSON)
		if err != nil {
			return err
		}
	}
	rec, err := attribution.Reconcile(sctx.Ctx, in, snapshot)
	if err != nil {
		return err
	}
	if err := persistAttribution(sctx, rec, false); err != nil {
		return err
	}
	if sctx.DB == nil {
		return nil
	}
	encoded, err := types.MarshalFindingsJSON(attribution.FindingsFrom(rec))
	if err != nil {
		return err
	}
	for _, step := range in.Steps {
		if step != nil && step.StepName == types.StepAttribution && step.ID != "" {
			return sctx.DB.SetStepFindings(step.ID, encoded)
		}
	}
	return nil
}

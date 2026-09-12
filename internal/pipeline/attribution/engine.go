package attribution

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Input is the durable pipeline/persistence view the engine needs. Tests drive
// it with real temporary Git repositories and recorded step/round rows.
type Input struct {
	WorkDir string
	Run     *db.Run
	Repo    *db.Repo
	Steps   []*db.StepResult
	Rounds  map[string][]*db.StepRound
	HeadSHA string
	PRURL   string
}

// Snapshot builds the pre-Lint observational record: changes since submission
// across preceding steps, attributed from confirmed findings and fix evidence.
func Snapshot(ctx context.Context, in Input) (*types.AttributionRecord, error) {
	return build(ctx, in, types.AttributionPhaseSnapshot, nil)
}

// Reconcile produces the final record from a snapshot plus later pipeline
// mutations (lint/CI/rebase). The snapshot itself is not rewritten.
func Reconcile(ctx context.Context, in Input, snapshot *types.AttributionRecord) (*types.AttributionRecord, error) {
	if snapshot == nil {
		return unavailableRecord(in, types.AttributionPhaseFinal, []string{"no attribution snapshot; refusing a clean score"}), nil
	}
	return build(ctx, in, types.AttributionPhaseFinal, snapshot)
}

func build(ctx context.Context, in Input, phase string, snapshot *types.AttributionRecord) (*types.AttributionRecord, error) {
	rec := newRecord(in, phase)
	if in.Run == nil {
		rec.Status = types.AttributionStatusUnavailable
		rec.EvidenceGaps = append(rec.EvidenceGaps, "run metadata is missing")
		return rec, nil
	}

	workerAdded, pipelineAdded, gaps := loadChangeMaps(ctx, in)
	rec.EvidenceGaps = append(rec.EvidenceGaps, gaps...)

	preLint := map[types.StepName]bool{
		types.StepIntent: true, types.StepRebase: true, types.StepReview: true,
		types.StepTest: true, types.StepDocument: true,
	}
	includeStep := func(name types.StepName) bool {
		if phase == types.AttributionPhaseSnapshot {
			return preLint[name] || name.IsCustomGate()
		}
		return name != types.StepAttribution
	}

	byFingerprint := make(map[string]*types.AttributedBug)
	var nonBugs []types.NonBugChange
	for _, step := range in.Steps {
		if step == nil || !includeStep(step.StepName) {
			continue
		}
		if step.Status == types.StepStatusSkipped || step.Status == types.StepStatusFailed {
			if step.StepName == types.StepReview || step.StepName == types.StepTest || step.StepName == types.StepDocument {
				rec.EvidenceGaps = append(rec.EvidenceGaps, fmt.Sprintf("%s step %s; not treated as a clean score", step.StepName, step.Status))
			}
		}
		rounds := in.Rounds[step.ID]
		for _, cf := range classifyStepFindings(step, rounds) {
			if cf.Kind != types.ChangeKindBug {
				nonBugs = append(nonBugs, types.NonBugChange{
					ID: cf.Finding.ID, Fingerprint: fingerprint(cf.Finding),
					File: cf.Finding.File, Line: cf.Finding.Line,
					Description: cf.Finding.Description, SourceStep: cf.SourceStep, Kind: cf.Kind,
				})
				continue
			}
			fp := fingerprint(cf.Finding)
			if existing, ok := byFingerprint[fp]; ok {
				if cf.Fixed {
					existing.Outcome = types.BugOutcomeFixedBeforeShipping
					existing.FixedInStep = cf.SourceStep
				}
				continue
			}
			bug := types.AttributedBug{
				ID:          cf.Finding.ID,
				Fingerprint: fp,
				File:        cf.Finding.File,
				Line:        cf.Finding.Line,
				Description: cf.Finding.Description,
				SourceStep:  cf.SourceStep,
			}
			bug.Attribution, bug.Confidence, bug.Evidence = attributeBug(cf, workerAdded, pipelineAdded)
			if cf.Fixed {
				bug.Outcome = types.BugOutcomeFixedBeforeShipping
				bug.FixedInStep = cf.SourceStep
			} else {
				bug.Outcome = types.BugOutcomeStillOpen
			}
			byFingerprint[fp] = &bug
		}
	}

	if snapshot != nil {
		for i := range snapshot.Bugs {
			fp := snapshot.Bugs[i].Fingerprint
			if fp == "" {
				fp = snapshot.Bugs[i].ID
			}
			if _, ok := byFingerprint[fp]; !ok {
				copied := snapshot.Bugs[i]
				byFingerprint[fp] = &copied
			}
		}
		for _, nb := range snapshot.NonBugs {
			nonBugs = append(nonBugs, nb)
		}
		rec.SnapshotHeadSHA = snapshot.SnapshotHeadSHA
		rec.Reconciled = strings.TrimSpace(in.HeadSHA) != "" && in.HeadSHA != snapshot.SnapshotHeadSHA
		if rec.Reconciled {
			rec.EvidenceGaps = append(rec.EvidenceGaps, "reconciled commits after the pre-lint snapshot")
		}
		if snapshot.BugFix != nil && rec.BugFix == nil {
			copied := *snapshot.BugFix
			rec.BugFix = &copied
		}
	}

	for _, bug := range byFingerprint {
		rec.Bugs = append(rec.Bugs, *bug)
	}
	rec.NonBugs = dedupeNonBugs(nonBugs)
	sortBugs(rec)
	types.RecalculateAttributionCounts(rec)
	attachBugFixLink(in, rec)
	setStatus(rec)
	return rec, nil
}

func newRecord(in Input, phase string) *types.AttributionRecord {
	rec := &types.AttributionRecord{
		SchemaVersion: types.AttributionSchemaVersion,
		Status:        types.AttributionStatusPartial,
		Phase:         phase,
		Limits:        types.DefaultAttributionLimits(),
		Bugs:          []types.AttributedBug{},
		NonBugs:       []types.NonBugChange{},
	}
	if in.Run != nil {
		rec.RunID = in.Run.ID
		rec.RepoID = in.Run.RepoID
		rec.Branch = in.Run.Branch
		if in.Run.SubmittedHeadSHA != nil {
			rec.SubmittedHeadSHA = *in.Run.SubmittedHeadSHA
		}
		worker, _ := types.ParseWorkerProvenance(deref(in.Run.WorkerProvenanceJSON))
		rec.Worker = types.WorkerIdentityFrom(worker)
		if worker != nil {
			rec.TaskID = worker.TaskID
			rec.ExternalRunID = worker.ExternalRunID
		}
		if in.Run.FixesRunID != nil {
			id := strings.TrimSpace(*in.Run.FixesRunID)
			if id != "" {
				rec.BugFix = &types.BugFixLink{OriginatingRunID: id, Confidence: types.AttributionConfidenceUnknown}
			}
		}
	}
	head := strings.TrimSpace(in.HeadSHA)
	if phase == types.AttributionPhaseSnapshot {
		rec.SnapshotHeadSHA = head
	} else {
		rec.FinalHeadSHA = head
	}
	rec.PRURL = strings.TrimSpace(in.PRURL)
	if rec.PRURL == "" && in.Run != nil && in.Run.PRURL != nil {
		rec.PRURL = strings.TrimSpace(*in.Run.PRURL)
	}
	return rec
}

func loadChangeMaps(ctx context.Context, in Input) (worker, pipeline map[string]map[int]struct{}, gaps []string) {
	if strings.TrimSpace(in.WorkDir) == "" {
		return nil, nil, []string{"worktree path is missing"}
	}
	submitted := ""
	base := ""
	head := strings.TrimSpace(in.HeadSHA)
	if in.Run != nil {
		if in.Run.SubmittedHeadSHA != nil {
			submitted = strings.TrimSpace(*in.Run.SubmittedHeadSHA)
		}
		base = strings.TrimSpace(in.Run.BaseSHA)
	}
	if submitted == "" {
		gaps = append(gaps, "submitted head SHA is missing")
	} else if !commitExists(ctx, in.WorkDir, submitted) {
		gaps = append(gaps, "submitted head SHA is not present in git")
		submitted = ""
	}
	if head == "" || !commitExists(ctx, in.WorkDir, head) {
		if live, err := git.HeadSHA(ctx, in.WorkDir); err == nil {
			head = strings.TrimSpace(live)
		}
	}
	if base != "" && (git.IsZeroSHA(base) || !commitExists(ctx, in.WorkDir, base)) {
		base = ""
	}

	if submitted != "" && base != "" && commitExists(ctx, in.WorkDir, base) {
		if diff, err := git.Diff(ctx, in.WorkDir, base, submitted); err == nil {
			worker = addedLines(diff)
		} else {
			gaps = append(gaps, "could not diff base..submitted")
		}
	} else if submitted != "" && base == "" {
		gaps = append(gaps, "base SHA is missing; worker vs pre-existing split is unknown")
	}

	if submitted != "" && head != "" {
		if !isAncestor(ctx, in.WorkDir, submitted, head) {
			gaps = append(gaps, "submitted head is not an ancestor of the current head after rewrite; git location is unknown")
		} else if diff, err := git.Diff(ctx, in.WorkDir, submitted, head); err == nil {
			pipeline = addedLines(diff)
		} else {
			gaps = append(gaps, "could not diff submitted..head")
		}
	}

	// Refine pipeline map with fix-round ranges when SHAs survive rewrite.
	for _, step := range in.Steps {
		for _, round := range in.Rounds[step.ID] {
			if round == nil || !round.IsFixRound() || round.StartingHeadSHA == nil {
				continue
			}
			from := strings.TrimSpace(*round.StartingHeadSHA)
			to := head
			if round.ReviewedHeadSHA != nil && strings.TrimSpace(*round.ReviewedHeadSHA) != "" {
				to = strings.TrimSpace(*round.ReviewedHeadSHA)
			}
			if !commitExists(ctx, in.WorkDir, from) || !commitExists(ctx, in.WorkDir, to) {
				continue
			}
			diff, err := git.Diff(ctx, in.WorkDir, from, to)
			if err != nil {
				continue
			}
			more := addedLines(diff)
			if pipeline == nil {
				pipeline = more
				continue
			}
			for path, lines := range more {
				if pipeline[path] == nil {
					pipeline[path] = lines
					continue
				}
				for line := range lines {
					pipeline[path][line] = struct{}{}
				}
			}
		}
	}
	return worker, pipeline, gaps
}

func attributeBug(cf classifiedFinding, worker, pipeline map[string]map[int]struct{}) (bucket, confidence string, evidence []string) {
	file, line := cf.Finding.File, cf.Finding.Line
	if file == "" || line <= 0 {
		return types.AttributionUnknown, types.AttributionConfidenceUnknown, []string{"finding has no file:line; refusing to infer from the diff"}
	}
	inPipeline := lineAddedIn(pipeline, file, line)
	inWorker := lineAddedIn(worker, file, line)
	switch {
	case inPipeline && !inWorker:
		return types.AttributionPipeline, types.AttributionConfidenceHigh, []string{"confirmed finding line was introduced after submission"}
	case inWorker && !inPipeline:
		return types.AttributionOriginalWorker, types.AttributionConfidenceHigh, []string{"confirmed finding line was introduced in the submitted change"}
	case inWorker && inPipeline:
		return types.AttributionUnknown, types.AttributionConfidenceLow, []string{"line appears in both submitted and post-submission diffs"}
	case fileInDiff(worker, file) || fileInDiff(pipeline, file):
		return types.AttributionPreExisting, types.AttributionConfidenceMedium, []string{"file was touched but the finding line was not an added line"}
	default:
		return types.AttributionPreExisting, types.AttributionConfidenceMedium, []string{"confirmed finding line was not added in the submitted or pipeline diffs"}
	}
}

func attachBugFixLink(in Input, rec *types.AttributionRecord) {
	if rec.BugFix == nil {
		return
	}
	reviewOK, testOK := stepCompleted(in, types.StepReview), stepCompleted(in, types.StepTest)
	rec.BugFix.Evidence = []string{"typed fixes_run_id signal"}
	if reviewOK && testOK {
		rec.BugFix.Confirmed = true
		rec.BugFix.Confidence = types.AttributionConfidenceHigh
		rec.BugFix.Evidence = append(rec.BugFix.Evidence, "review completed", "test completed")
		return
	}
	rec.BugFix.Confirmed = false
	rec.BugFix.Confidence = types.AttributionConfidenceUnknown
	if !reviewOK {
		rec.BugFix.Evidence = append(rec.BugFix.Evidence, "review did not complete; repair is unconfirmed")
		rec.EvidenceGaps = append(rec.EvidenceGaps, "bug-fix run did not complete review")
	}
	if !testOK {
		rec.BugFix.Evidence = append(rec.BugFix.Evidence, "test did not complete; repair is unconfirmed")
		rec.EvidenceGaps = append(rec.EvidenceGaps, "bug-fix run did not complete test")
	}
}

func stepCompleted(in Input, name types.StepName) bool {
	for _, step := range in.Steps {
		if step != nil && step.StepName == name {
			return step.Status == types.StepStatusCompleted
		}
	}
	return false
}

func setStatus(rec *types.AttributionRecord) {
	if rec == nil {
		return
	}
	if rec.SubmittedHeadSHA == "" && rec.Worker.Tool == types.WorkerFieldUnknown {
		rec.EvidenceGaps = appendUnique(rec.EvidenceGaps, "entry provenance is incomplete")
	}
	if len(rec.EvidenceGaps) == 0 {
		rec.Status = types.AttributionStatusComplete
		return
	}
	rec.Status = types.AttributionStatusPartial
}

func unavailableRecord(in Input, phase string, gaps []string) *types.AttributionRecord {
	rec := newRecord(in, phase)
	rec.Status = types.AttributionStatusUnavailable
	rec.EvidenceGaps = gaps
	types.RecalculateAttributionCounts(rec)
	return rec
}

func SkippedRecord(in Input) *types.AttributionRecord {
	rec := newRecord(in, types.AttributionPhaseSnapshot)
	rec.Status = types.AttributionStatusSkipped
	rec.EvidenceGaps = []string{"attribution step was skipped; not a clean score"}
	types.RecalculateAttributionCounts(rec)
	return rec
}

func commitExists(ctx context.Context, dir, sha string) bool {
	if strings.TrimSpace(sha) == "" || git.IsZeroSHA(sha) {
		return false
	}
	_, err := git.Run(ctx, dir, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func isAncestor(ctx context.Context, dir, anc, desc string) bool {
	if !commitExists(ctx, dir, anc) || !commitExists(ctx, dir, desc) {
		return false
	}
	_, err := git.Run(ctx, dir, "merge-base", "--is-ancestor", anc, desc)
	return err == nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func appendUnique(list []string, item string) []string {
	for _, existing := range list {
		if existing == item {
			return list
		}
	}
	return append(list, item)
}

func dedupeNonBugs(in []types.NonBugChange) []types.NonBugChange {
	seen := make(map[string]bool)
	var out []types.NonBugChange
	for _, nb := range in {
		key := nb.Fingerprint
		if key == "" {
			key = nb.ID
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, nb)
	}
	return out
}

func sortBugs(rec *types.AttributionRecord) {
	if rec == nil {
		return
	}
	// Stable, deterministic: source step order then file:line then id.
	bugs := rec.Bugs
	for i := 0; i < len(bugs); i++ {
		for j := i + 1; j < len(bugs); j++ {
			if bugLess(bugs[j], bugs[i]) {
				bugs[i], bugs[j] = bugs[j], bugs[i]
			}
		}
	}
}

func bugLess(a, b types.AttributedBug) bool {
	if a.SourceStep.Order() != b.SourceStep.Order() {
		return a.SourceStep.Order() < b.SourceStep.Order()
	}
	if a.File != b.File {
		return a.File < b.File
	}
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.ID < b.ID
}

// MarshalRecord is the persistence encoding for run columns and findings.
func MarshalRecord(rec *types.AttributionRecord) (string, error) {
	if rec == nil {
		return "", nil
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// ReconcileRun loads the run's steps and writes the final attribution record.
// A missing snapshot is stored as unavailable rather than a clean score.
// Skip this when the run never had an attribution step (legacy).
func ReconcileRun(ctx context.Context, database *db.DB, run *db.Run, repo *db.Repo, workDir string) error {
	if database == nil || run == nil {
		return nil
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		return err
	}
	var attrStep *db.StepResult
	for _, step := range steps {
		if step != nil && step.StepName == types.StepAttribution {
			attrStep = step
			break
		}
	}
	if attrStep == nil {
		return nil
	}
	in := Input{WorkDir: workDir, Run: run, Repo: repo, Steps: steps, Rounds: map[string][]*db.StepRound{}, HeadSHA: run.HeadSHA}
	if run.PRURL != nil {
		in.PRURL = *run.PRURL
	}
	for _, step := range steps {
		rounds, err := database.GetRoundsByStep(step.ID)
		if err != nil {
			return err
		}
		in.Rounds[step.ID] = rounds
	}
	if workDir != "" {
		if live, err := git.HeadSHA(ctx, workDir); err == nil && live != "" {
			in.HeadSHA = live
		}
	}
	var rec *types.AttributionRecord
	switch attrStep.Status {
	case types.StepStatusSkipped:
		rec = SkippedRecord(in)
	default:
		var snapshot *types.AttributionRecord
		if run.AttributionSnapshotJSON != nil {
			snapshot, err = UnmarshalRecord(*run.AttributionSnapshotJSON)
			if err != nil {
				return err
			}
		}
		rec, err = Reconcile(ctx, in, snapshot)
		if err != nil {
			return err
		}
	}
	raw, err := MarshalRecord(rec)
	if err != nil {
		return err
	}
	if err := database.SetRunAttributionFinal(run.ID, raw); err != nil {
		return err
	}
	run.AttributionJSON = &raw
	encoded, err := types.MarshalFindingsJSON(FindingsFrom(rec))
	if err != nil {
		return err
	}
	return database.SetStepFindings(attrStep.ID, encoded)
}

func UnmarshalRecord(raw string) (*types.AttributionRecord, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var rec types.AttributionRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil, fmt.Errorf("parse attribution record: %w", err)
	}
	return &rec, nil
}

func FindingsFrom(rec *types.AttributionRecord) types.Findings {
	summary := "attribution unavailable"
	if rec != nil {
		summary = fmt.Sprintf("attribution %s %s: worker=%d pipeline=%d pre_existing=%d unknown=%d non_bugs=%d",
			rec.Phase, rec.Status, rec.Counts.OriginalWorker, rec.Counts.Pipeline, rec.Counts.PreExisting, rec.Counts.Unknown, rec.Counts.NonBugs)
	}
	return types.Findings{
		Summary:       summary,
		RiskLevel:     "low",
		RiskRationale: "observational attribution; does not gate the run",
		Attribution:   rec,
	}
}

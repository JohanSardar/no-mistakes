package attribution

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Input is the durable pipeline/persistence view the engine needs. Tests drive
// it with real temporary Git repositories and recorded step/round rows.
//
// Run.BaseSHA is deliberately not read: the daemon stores the gate's previous
// head there (the zero SHA on a first push, the submitted head itself on an
// axi-run fallback), never the branch base. The engine resolves the base as
// every other step does, by merge-base against Repo.DefaultBranch.
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
// mutations (lint/CI/rebase). The snapshot itself is not rewritten: a bug it
// already attributed keeps that attribution, and only its outcome is re-read
// from the rounds.
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
	loc := newLocator(ctx, in)

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

	snapshotBugs := make(map[string]types.AttributedBug)
	if snapshot != nil {
		for _, bug := range snapshot.Bugs {
			snapshotBugs[bug.Fingerprint] = bug
		}
	}

	byFingerprint := make(map[string]*types.AttributedBug)
	var order []string
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
		for _, cf := range classifyStepFindings(step, in.Rounds[step.ID]) {
			fp := fingerprint(cf.Finding)
			if cf.Kind != types.ChangeKindBug {
				nonBugs = append(nonBugs, types.NonBugChange{
					ID: cf.Finding.ID, Fingerprint: fp,
					File: cf.Finding.File, Line: cf.Finding.Line,
					Description: cf.Finding.Description, SourceStep: cf.SourceStep, Kind: cf.Kind,
				})
				continue
			}
			if _, ok := byFingerprint[fp]; ok {
				continue
			}
			bug := types.AttributedBug{
				ID:          cf.Finding.ID,
				Fingerprint: fp,
				File:        cf.Finding.File,
				Line:        cf.Finding.Line,
				Description: cf.Finding.Description,
				SourceStep:  cf.SourceStep,
				Outcome:     cf.Outcome,
			}
			if cf.Outcome == types.BugOutcomeFixedBeforeShipping {
				bug.FixedInStep = cf.SourceStep
			}
			if prior, ok := snapshotBugs[fp]; ok {
				bug.Attribution, bug.Confidence, bug.Evidence = prior.Attribution, prior.Confidence, prior.Evidence
			} else {
				bug.Attribution, bug.Confidence, bug.Evidence = loc.attribute(cf)
			}
			byFingerprint[fp] = &bug
			order = append(order, fp)
		}
	}

	if snapshot != nil {
		for _, bug := range snapshot.Bugs {
			if _, ok := byFingerprint[bug.Fingerprint]; ok {
				continue
			}
			copied := bug
			byFingerprint[bug.Fingerprint] = &copied
			order = append(order, bug.Fingerprint)
		}
		nonBugs = append(nonBugs, snapshot.NonBugs...)
		rec.SnapshotHeadSHA = snapshot.SnapshotHeadSHA
		rec.Reconciled = strings.TrimSpace(in.HeadSHA) != "" && in.HeadSHA != snapshot.SnapshotHeadSHA
		if rec.Reconciled {
			rec.EvidenceGaps = append(rec.EvidenceGaps, "reconciled commits after the pre-lint snapshot")
		}
	}
	rec.EvidenceGaps = append(rec.EvidenceGaps, loc.gaps...)

	shipped := phase == types.AttributionPhaseFinal && in.Run.Status == types.RunCompleted
	for _, fp := range order {
		bug := byFingerprint[fp]
		if shipped && bug.Outcome == types.BugOutcomeStillOpen {
			bug.Outcome = types.BugOutcomeEscaped
		}
		rec.Bugs = append(rec.Bugs, *bug)
	}
	rec.NonBugs = dedupeNonBugs(nonBugs)
	sort.SliceStable(rec.Bugs, func(i, j int) bool { return bugLess(rec.Bugs[i], rec.Bugs[j]) })
	types.RecalculateAttributionCounts(rec)
	attachBugFixLink(in, rec, phase)
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

// locator answers "who added this line" for a finding in the coordinates of
// the commit that finding was reviewed at. Two diffs, both expressed at that
// commit, partition its lines: workerHead..locate is what the pipeline wrote
// after submission, base..locate is everything the branch introduced, and the
// remainder of the branch set is the original worker's. Neither diff alone is
// causation; only a confirmed finding on such a line is attributed.
type locator struct {
	ctx           context.Context
	dir           string
	head          string
	submitted     string
	defaultBranch string
	// reviewHeads are the commits review rounds examined, in round order.
	// When a rebase rewrote the submitted head before review, the first of
	// these that is an ancestor of a finding's commit stands in for it.
	reviewHeads []string
	anchors     map[string]*anchor
	diffs       map[string]map[string]map[int]struct{}
	unavailable string
	gaps        []string
}

type anchor struct {
	workerHead string
	base       string
	rewritten  bool
	err        string
}

func newLocator(ctx context.Context, in Input) *locator {
	l := &locator{ctx: ctx, dir: strings.TrimSpace(in.WorkDir), anchors: map[string]*anchor{}, diffs: map[string]map[string]map[int]struct{}{}}
	if in.Repo != nil {
		l.defaultBranch = strings.TrimSpace(in.Repo.DefaultBranch)
	}
	if l.dir == "" {
		l.unavailable = "worktree path is missing; git location is unknown"
		l.gaps = append(l.gaps, l.unavailable)
		return l
	}
	if in.Run != nil && in.Run.SubmittedHeadSHA != nil {
		l.submitted = strings.TrimSpace(*in.Run.SubmittedHeadSHA)
	}
	switch {
	case l.submitted == "":
		l.unavailable = "submitted head SHA is missing; git location is unknown"
	case !commitExists(ctx, l.dir, l.submitted):
		l.unavailable = "submitted head SHA is not present in git; git location is unknown"
	}
	if l.unavailable != "" {
		l.gaps = append(l.gaps, l.unavailable)
		return l
	}
	l.head = strings.TrimSpace(in.HeadSHA)
	if l.head == "" || !commitExists(ctx, l.dir, l.head) {
		if live, err := git.HeadSHA(ctx, l.dir); err == nil {
			l.head = strings.TrimSpace(live)
		}
	}
	for _, step := range in.Steps {
		if step == nil || step.StepName != types.StepReview {
			continue
		}
		for _, round := range in.Rounds[step.ID] {
			if round == nil {
				continue
			}
			if sha := strings.TrimSpace(deref(round.ReviewedHeadSHA)); sha != "" {
				l.reviewHeads = append(l.reviewHeads, sha)
			}
		}
	}
	return l
}

func (l *locator) attribute(cf classifiedFinding) (bucket, confidence string, evidence []string) {
	file, line := cf.Finding.File, cf.Finding.Line
	if file == "" || line <= 0 {
		return types.AttributionUnknown, types.AttributionConfidenceUnknown, []string{"finding has no file:line; refusing to infer from the diff"}
	}
	if l.unavailable != "" {
		return types.AttributionUnknown, types.AttributionConfidenceUnknown, []string{l.unavailable}
	}
	locate := cf.LocateSHA
	if locate == "" || !commitExists(l.ctx, l.dir, locate) {
		locate = l.head
	}
	if locate == "" {
		return types.AttributionUnknown, types.AttributionConfidenceUnknown, []string{"no commit records where the finding was located"}
	}
	a := l.anchorFor(locate)
	if a.err != "" {
		return types.AttributionUnknown, types.AttributionConfidenceUnknown, []string{a.err}
	}
	confidence = types.AttributionConfidenceHigh
	if a.rewritten {
		confidence = types.AttributionConfidenceMedium
	}
	pipelineAdded, err := l.added(a.workerHead, locate)
	if err != nil {
		return types.AttributionUnknown, types.AttributionConfidenceUnknown, []string{fmt.Sprintf("could not diff %s..%s", short(a.workerHead), short(locate))}
	}
	if lineAddedIn(pipelineAdded, file, line) {
		return types.AttributionPipeline, confidence, []string{fmt.Sprintf("line %s:%d at %s was added after %s", file, line, short(locate), short(a.workerHead))}
	}
	if a.base == "" {
		return types.AttributionUnknown, types.AttributionConfidenceUnknown, []string{fmt.Sprintf("branch base of %s could not be resolved against %q; worker vs pre-existing split is unknown", short(a.workerHead), l.defaultBranch)}
	}
	branchAdded, err := l.added(a.base, locate)
	if err != nil {
		return types.AttributionUnknown, types.AttributionConfidenceUnknown, []string{fmt.Sprintf("could not diff %s..%s", short(a.base), short(locate))}
	}
	if lineAddedIn(branchAdded, file, line) {
		return types.AttributionOriginalWorker, confidence, []string{fmt.Sprintf("line %s:%d at %s was added by the submitted change (%s..%s)", file, line, short(locate), short(a.base), short(a.workerHead))}
	}
	return types.AttributionPreExisting, types.AttributionConfidenceMedium, []string{fmt.Sprintf("line %s:%d at %s predates the branch base %s", file, line, short(locate), short(a.base))}
}

func (l *locator) anchorFor(locate string) *anchor {
	if a, ok := l.anchors[locate]; ok {
		return a
	}
	a := &anchor{}
	l.anchors[locate] = a
	switch {
	case isAncestor(l.ctx, l.dir, l.submitted, locate):
		a.workerHead = l.submitted
	default:
		for _, sha := range l.reviewHeads {
			if isAncestor(l.ctx, l.dir, sha, locate) {
				a.workerHead, a.rewritten = sha, true
				break
			}
		}
	}
	if a.workerHead == "" {
		a.err = fmt.Sprintf("submitted head is not an ancestor of %s after a history rewrite; git location is unknown", short(locate))
		l.gaps = appendUnique(l.gaps, a.err)
		return a
	}
	if a.rewritten {
		l.gaps = appendUnique(l.gaps, fmt.Sprintf("submitted head was rewritten before review; worker anchor for %s is the head review first examined (%s)", short(locate), short(a.workerHead)))
	}
	a.base = l.mergeBase(a.workerHead)
	if a.base == "" {
		l.gaps = appendUnique(l.gaps, fmt.Sprintf("branch base of %s could not be resolved against %q; worker vs pre-existing split is unknown", short(a.workerHead), l.defaultBranch))
	}
	return a
}

func (l *locator) mergeBase(sha string) string {
	if l.defaultBranch == "" {
		return ""
	}
	for _, ref := range []string{"origin/" + l.defaultBranch, l.defaultBranch} {
		mb, err := git.Run(l.ctx, l.dir, "merge-base", sha, ref)
		if err == nil && strings.TrimSpace(mb) != "" {
			return strings.TrimSpace(mb)
		}
	}
	return ""
}

func (l *locator) added(from, to string) (map[string]map[int]struct{}, error) {
	key := from + ".." + to
	if cached, ok := l.diffs[key]; ok {
		return cached, nil
	}
	added, err := diffAdded(l.ctx, l.dir, from, to)
	if err != nil {
		return nil, err
	}
	l.diffs[key] = added
	return added, nil
}

// attachBugFixLink confirms the typed fixes_run_id link only once the repair
// actually shipped: review and test completed and the run itself completed.
// A snapshot is taken mid-run, and a failed or cancelled run never shipped,
// so both stay unconfirmed with the reason recorded.
func attachBugFixLink(in Input, rec *types.AttributionRecord, phase string) {
	if rec.BugFix == nil {
		return
	}
	rec.BugFix.Evidence = []string{"typed fixes_run_id signal"}
	rec.BugFix.Confirmed = false
	rec.BugFix.Confidence = types.AttributionConfidenceUnknown
	var unmet []string
	if !stepCompleted(in, types.StepReview) {
		unmet = append(unmet, "review did not complete")
	}
	if !stepCompleted(in, types.StepTest) {
		unmet = append(unmet, "test did not complete")
	}
	switch {
	case phase != types.AttributionPhaseFinal:
		unmet = append(unmet, "run has not finished")
	case in.Run.Status != types.RunCompleted:
		unmet = append(unmet, fmt.Sprintf("run %s; repair did not ship", in.Run.Status))
	}
	if len(unmet) == 0 {
		rec.BugFix.Confirmed = true
		rec.BugFix.Confidence = types.AttributionConfidenceHigh
		rec.BugFix.Evidence = append(rec.BugFix.Evidence, "review completed", "test completed", "run completed")
		return
	}
	for _, reason := range unmet {
		rec.BugFix.Evidence = append(rec.BugFix.Evidence, reason+"; repair is unconfirmed")
		rec.EvidenceGaps = append(rec.EvidenceGaps, "bug-fix run: "+reason)
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

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
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
	if a.ID != b.ID {
		return a.ID < b.ID
	}
	return a.Fingerprint < b.Fingerprint
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
		summary = fmt.Sprintf("attribution %s %s: worker=%d pipeline=%d pre_existing=%d unknown=%d non_bugs=%d fixed=%d escaped=%d",
			rec.Phase, rec.Status, rec.Counts.OriginalWorker, rec.Counts.Pipeline, rec.Counts.PreExisting, rec.Counts.Unknown, rec.Counts.NonBugs, rec.Counts.FixedBeforeShipping, rec.Counts.Escaped)
	}
	return types.Findings{
		Summary:       summary,
		RiskLevel:     "low",
		RiskRationale: "observational attribution; does not gate the run",
		Attribution:   rec,
	}
}

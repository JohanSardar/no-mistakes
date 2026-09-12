package types

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Attribution schema and vocabularies. A changed line is never itself a bug:
// only confirmed findings/fix evidence can populate AttributedBug, and
// insufficient evidence stays unknown.

const AttributionSchemaVersion = 1

const (
	AttributionPhaseSnapshot = "snapshot"
	AttributionPhaseFinal    = "final"
)

const (
	AttributionStatusComplete    = "complete"
	AttributionStatusPartial     = "partial"
	AttributionStatusSkipped     = "skipped"
	AttributionStatusUnavailable = "unavailable"
)

const (
	AttributionOriginalWorker = "original_worker"
	AttributionPipeline       = "pipeline"
	AttributionPreExisting    = "pre_existing"
	AttributionUnknown        = "unknown"
)

const (
	BugOutcomeFixedBeforeShipping = "fixed_before_shipping"
	BugOutcomeEscaped             = "escaped"
	BugOutcomeStillOpen           = "still_open"
	BugOutcomeUnknown             = "unknown"
)

const (
	ChangeKindBug   = "bug"
	ChangeKindDocs  = "docs"
	ChangeKindStyle = "style"
	ChangeKindOther = "other"
)

const (
	AttributionConfidenceHigh    = "high"
	AttributionConfidenceMedium  = "medium"
	AttributionConfidenceUnknown = "unknown"
)

const WorkerFieldUnknown = "unknown"

// LaunchAttribution is the optional entry provenance a caller supplies when
// starting a run. Absent fields stay unknown. FixesRunID is the only supported
// signal that this run is a later bug-fix of an earlier run: a non-empty value
// that names a real run in the same repository. Free-text intent is never that
// signal.
type LaunchAttribution struct {
	Worker     *WorkerProvenance `json:"worker,omitempty"`
	FixesRunID string            `json:"fixes_run_id,omitempty"`
}

// WorkerProvenance is the actual originating worker identity as supplied or
// independently verified. Empty strings are unknown; callers must not invent
// tool, model, provider, or settings values.
type WorkerProvenance struct {
	TaskID        string          `json:"task_id,omitempty"`
	ExternalRunID string          `json:"external_run_id,omitempty"`
	Tool          string          `json:"tool,omitempty"`
	Model         string          `json:"model,omitempty"`
	Provider      string          `json:"provider,omitempty"`
	Settings      json.RawMessage `json:"settings,omitempty"`
}

// AttributionRecord is the durable observational record for one run. Snapshot
// is taken after Documentation and before Lint; Final is the reconciled copy
// after later pipeline mutations. A later bug-fix run never rewrites an
// earlier run's snapshot or final evidence.
type AttributionRecord struct {
	SchemaVersion    int               `json:"schema_version"`
	Status           string            `json:"status"`
	Phase            string            `json:"phase"`
	RunID            string            `json:"run_id"`
	RepoID           string            `json:"repo_id"`
	Branch           string            `json:"branch,omitempty"`
	TaskID           string            `json:"task_id,omitempty"`
	ExternalRunID    string            `json:"external_run_id,omitempty"`
	SubmittedHeadSHA string            `json:"submitted_head_sha,omitempty"`
	SnapshotHeadSHA  string            `json:"snapshot_head_sha,omitempty"`
	FinalHeadSHA     string            `json:"final_head_sha,omitempty"`
	PRURL            string            `json:"pr_url,omitempty"`
	Worker           WorkerIdentity    `json:"worker"`
	BugFix           *BugFixLink       `json:"bug_fix,omitempty"`
	Bugs             []AttributedBug   `json:"bugs"`
	NonBugs          []NonBugChange    `json:"non_bugs"`
	Counts           AttributionCounts `json:"counts"`
	EvidenceGaps     []string          `json:"evidence_gaps,omitempty"`
	Reconciled       bool              `json:"reconciled,omitempty"`
	Limits           []string          `json:"limits,omitempty"`
}

// WorkerIdentity is what the record exposes: supplied values, otherwise
// unknown. Never inferred from git authors or the pipeline agent.
type WorkerIdentity struct {
	Tool     string          `json:"tool"`
	Model    string          `json:"model"`
	Provider string          `json:"provider"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// BugFixLink is present only when the run was started with the typed
// --fixes-run / fixes_run_id signal. Confirmed is the single verdict: the
// repair shipped (review and test completed, run completed) or it did not.
// The originating run's own record is not rewritten.
type BugFixLink struct {
	OriginatingRunID string   `json:"originating_run_id"`
	Confirmed        bool     `json:"confirmed"`
	Evidence         []string `json:"evidence,omitempty"`
}

// AttributedBug is one confirmed bug finding (not a touched line).
type AttributedBug struct {
	ID          string   `json:"id"`
	Fingerprint string   `json:"fingerprint"`
	File        string   `json:"file,omitempty"`
	Line        int      `json:"line,omitempty"`
	Description string   `json:"description"`
	SourceStep  StepName `json:"source_step"`
	Attribution string   `json:"attribution"`
	Outcome     string   `json:"outcome"`
	Confidence  string   `json:"confidence"`
	Evidence    []string `json:"evidence,omitempty"`
}

// NonBugChange records docs/style/other findings so they are explicitly not
// counted as bugs.
type NonBugChange struct {
	ID          string   `json:"id"`
	Fingerprint string   `json:"fingerprint"`
	File        string   `json:"file,omitempty"`
	Line        int      `json:"line,omitempty"`
	Description string   `json:"description"`
	SourceStep  StepName `json:"source_step"`
	Kind        string   `json:"kind"`
}

// AttributionCounts is the machine-readable tally. Skipped or failed evidence
// never appears as a clean zero without Status/EvidenceGaps saying so.
type AttributionCounts struct {
	OriginalWorker      int `json:"original_worker"`
	Pipeline            int `json:"pipeline"`
	PreExisting         int `json:"pre_existing"`
	Unknown             int `json:"unknown"`
	NonBugs             int `json:"non_bugs"`
	FixedBeforeShipping int `json:"fixed_before_shipping"`
	Escaped             int `json:"escaped"`
}

// ParseWorkerProvenance decodes caller-supplied worker identity. Empty input
// is valid and means every field is unknown. Invalid JSON is refused.
func ParseWorkerProvenance(raw string) (*WorkerProvenance, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var p WorkerProvenance
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse worker provenance: %w", err)
	}
	p.Normalize()
	if p.IsEmpty() {
		return nil, nil
	}
	return &p, nil
}

func (p *WorkerProvenance) Normalize() {
	if p == nil {
		return
	}
	p.TaskID = strings.TrimSpace(p.TaskID)
	p.ExternalRunID = strings.TrimSpace(p.ExternalRunID)
	p.Tool = strings.TrimSpace(p.Tool)
	p.Model = strings.TrimSpace(p.Model)
	p.Provider = strings.TrimSpace(p.Provider)
	if !isJSONObject(p.Settings) {
		p.Settings = nil
	}
}

func (p *WorkerProvenance) IsEmpty() bool {
	if p == nil {
		return true
	}
	return p.TaskID == "" && p.ExternalRunID == "" && p.Tool == "" && p.Model == "" && p.Provider == "" && len(p.Settings) == 0
}

func (a LaunchAttribution) IsBugFixRun() bool {
	return strings.TrimSpace(a.FixesRunID) != ""
}

func (a LaunchAttribution) IsEmpty() bool {
	return !a.IsBugFixRun() && (a.Worker == nil || a.Worker.IsEmpty())
}

func (a *LaunchAttribution) Normalize() {
	if a == nil {
		return
	}
	a.FixesRunID = strings.TrimSpace(a.FixesRunID)
	if a.Worker != nil {
		a.Worker.Normalize()
		if a.Worker.IsEmpty() {
			a.Worker = nil
		}
	}
}

func WorkerIdentityFrom(p *WorkerProvenance) WorkerIdentity {
	id := WorkerIdentity{
		Tool:     WorkerFieldUnknown,
		Model:    WorkerFieldUnknown,
		Provider: WorkerFieldUnknown,
	}
	if p == nil {
		return id
	}
	if p.Tool != "" {
		id.Tool = p.Tool
	}
	if p.Model != "" {
		id.Model = p.Model
	}
	if p.Provider != "" {
		id.Provider = p.Provider
	}
	if isJSONObject(p.Settings) {
		id.Settings = append(json.RawMessage(nil), p.Settings...)
	}
	return id
}

func isJSONObject(raw json.RawMessage) bool {
	trim := bytes.TrimSpace(raw)
	if len(trim) < 2 || trim[0] != '{' || trim[len(trim)-1] != '}' {
		return false
	}
	var obj map[string]json.RawMessage
	return json.Unmarshal(trim, &obj) == nil
}

func RecalculateAttributionCounts(rec *AttributionRecord) {
	if rec == nil {
		return
	}
	var c AttributionCounts
	for _, bug := range rec.Bugs {
		switch bug.Attribution {
		case AttributionOriginalWorker:
			c.OriginalWorker++
		case AttributionPipeline:
			c.Pipeline++
		case AttributionPreExisting:
			c.PreExisting++
		default:
			c.Unknown++
		}
		switch bug.Outcome {
		case BugOutcomeFixedBeforeShipping:
			c.FixedBeforeShipping++
		case BugOutcomeEscaped:
			c.Escaped++
		}
	}
	c.NonBugs = len(rec.NonBugs)
	rec.Counts = c
}

func DefaultAttributionLimits() []string {
	return []string{
		"A changed line is not a bug; only confirmed findings and fix-round evidence are attributed.",
		"Git blame, last author, and the mere existence of a diff are not used as causation.",
		"Skipped or failed steps and missing metadata are never a clean score.",
		"A later bug-fix run does not rewrite the originating run's historical evidence.",
	}
}

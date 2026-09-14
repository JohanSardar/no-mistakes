package attribution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type classifiedFinding struct {
	Finding    types.Finding
	SourceStep types.StepName
	Kind       string // bug or a ChangeKind*
	// Outcome is set for bugs only: still open in the step's final round,
	// fixed before shipping, or unknown when it vanished without a fix.
	Outcome string
	// LocateSHA is the commit whose coordinates Finding.File/Line use; empty
	// when the round recorded none.
	LocateSHA string
}

type roundFindings struct {
	items    []types.Finding
	selected map[string]bool
	locate   string
	isFix    bool
	repair   bool
}

// classifyStepFindings folds a step's rounds into one entry per finding
// fingerprint. Finding IDs are positional labels the executor reassigns every
// round, so identity across rounds is content, never the ID; a round's
// SelectedFindingIDs are joined against that round's own findings only. Paths
// are made worktree-relative here, once, so identity, location, and the diff
// lookup all see the same key whichever form a round reported.
func classifyStepFindings(step *db.StepResult, rounds []*db.StepRound, dir string) []classifiedFinding {
	if step == nil {
		return nil
	}
	var parsed []roundFindings
	for _, round := range rounds {
		if round == nil {
			continue
		}
		findings := parseFindings(round.FindingsJSON)
		locate := strings.TrimSpace(deref(round.ReviewedHeadSHA))
		if locate == "" {
			locate = strings.TrimSpace(findings.TestedHeadSHA)
		}
		parsed = append(parsed, roundFindings{
			items:    relativeFiles(dir, withUserFindings(findings.Items, parseFindings(round.UserFindingsJSON).Items)),
			selected: selectedIDs(round),
			locate:   locate,
			isFix:    round.IsFixRound(),
			repair:   round.RepairPublished,
		})
	}
	if len(parsed) == 0 {
		parsed = []roundFindings{{items: relativeFiles(dir, parseFindings(step.FindingsJSON).Items), selected: map[string]bool{}}}
	}
	final := make(map[string]bool)
	finalLocations := make(map[string]bool)
	finalSubjects := make(map[string]bool)
	for _, item := range parsed[len(parsed)-1].items {
		final[fingerprint(item)] = true
		if loc := location(item); loc != "" {
			finalLocations[loc] = true
		}
		if s := subject(item); s != "" {
			finalSubjects[s] = true
		}
	}

	var out []classifiedFinding
	index := make(map[string]int)
	// subjects maps a finding's file and wording to its entry so a later
	// round's same-worded report at another line (the fix round inserted or
	// removed lines above it) folds into the finding it moved, never a second
	// bug; within one round two such reports are two findings.
	subjects := make(map[string]int)
	for i, rf := range parsed {
		fixFollows, repairFollows := false, false
		for _, later := range parsed[i+1:] {
			if later.isFix {
				fixFollows = true
				repairFollows = repairFollows || later.repair
			}
		}
		for _, item := range rf.items {
			fp := fingerprint(item)
			selected := rf.selected[item.ID]
			kind := findingKind(step.StepName, item, selected, repairFollows)
			// The final round still reporting the same wording in the file,
			// at any line, means the finding is still open; a reworded
			// re-report at the same file:line is not proof the fix landed
			// either, and that outcome stays unknown rather than credited.
			reported := final[fp] || finalSubjects[subject(item)]
			fixed := selected && fixFollows && !reported && !finalLocations[location(item)]
			j, ok := index[fp]
			if !ok {
				j, ok = subjects[subject(item)]
			}
			if ok {
				index[fp] = j
				if kind == types.ChangeKindBug {
					out[j].Kind = kind
				}
				if fixed {
					out[j].Outcome = types.BugOutcomeFixedBeforeShipping
				}
				continue
			}
			cf := classifiedFinding{Finding: item, SourceStep: step.StepName, Kind: kind, LocateSHA: rf.locate}
			switch {
			case reported:
				cf.Outcome = types.BugOutcomeStillOpen
			case fixed:
				cf.Outcome = types.BugOutcomeFixedBeforeShipping
			default:
				cf.Outcome = types.BugOutcomeUnknown
			}
			index[fp] = len(out)
			out = append(out, cf)
		}
		for _, item := range rf.items {
			if s := subject(item); s != "" {
				if _, seen := subjects[s]; !seen {
					subjects[s] = index[fingerprint(item)]
				}
			}
		}
	}
	return out
}

// findingKind decides whether a finding is a confirmed bug. A CI check is a
// bug only with evidence that the code was wrong: the finding was selected
// for repair and a later fix round published a code change. A red job on its
// own (a flaky runner, a tool version drift) is not.
func findingKind(step types.StepName, item types.Finding, selected, repairPublished bool) string {
	switch item.Category {
	case types.FindingCategoryDocumentation:
		return types.ChangeKindDocs
	case types.FindingCategoryLint:
		return types.ChangeKindStyle
	case types.FindingCategoryCIMergeConflict, types.FindingCategoryCITransient, types.FindingCategoryCIReviewBot, types.FindingCategoryTestVerdict:
		return types.ChangeKindOther
	case types.FindingCategoryCICheck:
		if selected && repairPublished {
			return types.ChangeKindBug
		}
		return types.ChangeKindOther
	}
	switch step {
	case types.StepDocument:
		return types.ChangeKindDocs
	case types.StepLint:
		return types.ChangeKindStyle
	}
	if item.ReviewScope == types.FindingReviewScopePipelineOwnedDelivery {
		return types.ChangeKindOther
	}
	sev := types.NormalizeFindingSeverity(item.Severity)
	if sev != types.FindingSeverityError && sev != types.FindingSeverityWarning && !(item.Source == types.FindingSourceUser && selected) {
		return types.ChangeKindOther
	}
	if step != types.StepReview && step != types.StepTest {
		return types.ChangeKindOther
	}
	act := item.ActionOrDefault()
	if act == types.ActionAutoFix || (selected && act != types.ActionNoOp) {
		return types.ChangeKindBug
	}
	// An ask-user finding is a design/intent question unless a fix round
	// actually selected it.
	return types.ChangeKindOther
}

func parseFindings(raw *string) types.Findings {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return types.Findings{}
	}
	findings, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		return types.Findings{}
	}
	return findings
}

// withUserFindings appends the findings a human authored at the fix gate
// (StepRound.UserFindingsJSON) to the round's own; the agent findings that
// list repeats are already present under the same IDs.
func withUserFindings(items, user []types.Finding) []types.Finding {
	if len(user) == 0 {
		return items
	}
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		seen[item.ID] = true
	}
	for _, item := range user {
		if item.ID == "" || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		items = append(items, item)
	}
	return items
}

// relativeFiles rewrites each finding's path relative to the worktree. A path
// outside it is kept verbatim so the locator can refuse it.
func relativeFiles(dir string, items []types.Finding) []types.Finding {
	for i := range items {
		items[i].File, _ = relativePath(dir, items[i].File)
	}
	return items
}

func relativePath(dir, file string) (string, bool) {
	file = strings.TrimSpace(file)
	if file == "" {
		return "", true
	}
	if filepath.IsAbs(file) {
		rel, err := filepath.Rel(dir, file)
		if dir == "" || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return file, false
		}
		file = rel
	}
	return path.Clean(strings.ReplaceAll(filepath.ToSlash(file), "\\", "/")), true
}

func selectedIDs(round *db.StepRound) map[string]bool {
	out := make(map[string]bool)
	if round == nil || round.SelectedFindingIDs == nil {
		return out
	}
	var ids []string
	if json.Unmarshal([]byte(*round.SelectedFindingIDs), &ids) != nil {
		return out
	}
	for _, id := range ids {
		if id != "" {
			out[id] = true
		}
	}
	return out
}

// fingerprint is a finding's identity across rounds. A CI check is identified
// by its check name alone: the description embeds the provider's details link
// and CheckID is the provider's per-execution object ID (the CI monitor's own
// execution discriminator), and both change on every rerun of the same red
// check.
func fingerprint(item types.Finding) string {
	parts := []string{
		strings.TrimSpace(item.File),
		strconv.Itoa(item.Line),
		wording(item),
	}
	if item.Category == types.FindingCategoryCICheck {
		parts = []string{item.Category, strings.TrimSpace(item.Check)}
	}
	norm := strings.ToLower(strings.Join(parts, "|"))
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])[:16]
}

func wording(item types.Finding) string {
	return strings.Join(strings.Fields(item.Description), " ")
}

// subject is a finding's file and wording without the line. A CI check has
// no file and is identified by its check name instead.
func subject(item types.Finding) string {
	file := strings.TrimSpace(item.File)
	if file == "" || item.Category == types.FindingCategoryCICheck {
		return ""
	}
	desc := wording(item)
	if desc == "" {
		return ""
	}
	return strings.ToLower(file + "|" + desc)
}

func location(item types.Finding) string {
	file := strings.TrimSpace(item.File)
	if file == "" || item.Line <= 0 {
		return ""
	}
	return strings.ToLower(file) + "|" + strconv.Itoa(item.Line)
}

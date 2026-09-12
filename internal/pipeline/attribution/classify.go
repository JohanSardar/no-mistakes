package attribution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// SelectedFindingIDs are joined against that round's own findings only.
func classifyStepFindings(step *db.StepResult, rounds []*db.StepRound) []classifiedFinding {
	if step == nil {
		return nil
	}
	var parsed []roundFindings
	for _, round := range rounds {
		if round == nil {
			continue
		}
		parsed = append(parsed, roundFindings{
			items:    parseFindingItems(round.FindingsJSON),
			selected: selectedIDs(round),
			locate:   deref(round.ReviewedHeadSHA),
			isFix:    round.IsFixRound(),
			repair:   round.RepairPublished,
		})
	}
	if len(parsed) == 0 {
		parsed = []roundFindings{{items: parseFindingItems(step.FindingsJSON), selected: map[string]bool{}}}
	}
	final := make(map[string]bool)
	for _, item := range parsed[len(parsed)-1].items {
		final[fingerprint(item)] = true
	}

	var out []classifiedFinding
	index := make(map[string]int)
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
			fixed := selected && fixFollows && !final[fp]
			if j, ok := index[fp]; ok {
				if kind == types.ChangeKindBug {
					out[j].Kind = kind
				}
				if fixed {
					out[j].Outcome = types.BugOutcomeFixedBeforeShipping
				}
				continue
			}
			cf := classifiedFinding{Finding: item, SourceStep: step.StepName, Kind: kind, LocateSHA: strings.TrimSpace(rf.locate)}
			switch {
			case final[fp]:
				cf.Outcome = types.BugOutcomeStillOpen
			case fixed:
				cf.Outcome = types.BugOutcomeFixedBeforeShipping
			default:
				cf.Outcome = types.BugOutcomeUnknown
			}
			index[fp] = len(out)
			out = append(out, cf)
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
	case types.FindingCategoryCIMergeConflict, types.FindingCategoryCITransient, types.FindingCategoryCIReviewBot:
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
	if sev != types.FindingSeverityError && sev != types.FindingSeverityWarning {
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

func parseFindingItems(raw *string) []types.Finding {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil
	}
	findings, err := types.ParseFindingsJSON(*raw)
	if err != nil {
		return nil
	}
	return findings.Items
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

func fingerprint(item types.Finding) string {
	norm := strings.ToLower(strings.Join([]string{
		strings.TrimSpace(item.File),
		strconv.Itoa(item.Line),
		strings.Join(strings.Fields(strings.TrimSpace(item.Description)), " "),
	}, "|"))
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])[:16]
}

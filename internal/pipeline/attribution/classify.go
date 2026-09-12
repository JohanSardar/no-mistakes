package attribution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type classifiedFinding struct {
	Finding    types.Finding
	SourceStep types.StepName
	Kind       string // bug or a ChangeKind*
	Selected   bool
	Fixed      bool
	LocateSHA  string
}

func classifyStepFindings(step *db.StepResult, rounds []*db.StepRound) []classifiedFinding {
	if step == nil {
		return nil
	}
	selected := selectedIDs(rounds)
	fixed := fixedIDs(rounds, selected)
	var out []classifiedFinding
	seen := make(map[string]bool)
	appendFrom := func(raw *string, locateSHA string) {
		if raw == nil || strings.TrimSpace(*raw) == "" {
			return
		}
		findings, err := types.ParseFindingsJSON(*raw)
		if err != nil {
			return
		}
		for _, item := range findings.Items {
			key := item.ID
			if key == "" {
				key = fingerprint(item)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			kind := findingKind(step.StepName, item)
			sel, wasFixed := selected[item.ID], fixed[item.ID]
			if kind != types.ChangeKindBug && (sel || wasFixed) && isCorrectnessFinding(step.StepName, item) {
				kind = types.ChangeKindBug
			}
			out = append(out, classifiedFinding{
				Finding:    item,
				SourceStep: step.StepName,
				Kind:       kind,
				Selected:   sel,
				Fixed:      wasFixed,
				LocateSHA:  locateSHA,
			})
		}
	}
	for _, round := range rounds {
		locate := ""
		if round.ReviewedHeadSHA != nil {
			locate = strings.TrimSpace(*round.ReviewedHeadSHA)
		}
		if locate == "" && round.StartingHeadSHA != nil {
			locate = strings.TrimSpace(*round.StartingHeadSHA)
		}
		appendFrom(round.FindingsJSON, locate)
	}
	appendFrom(step.FindingsJSON, "")
	return out
}

func findingKind(step types.StepName, item types.Finding) string {
	switch item.Category {
	case types.FindingCategoryDocumentation:
		return types.ChangeKindDocs
	case types.FindingCategoryLint:
		return types.ChangeKindStyle
	case types.FindingCategoryCIMergeConflict, types.FindingCategoryCITransient, types.FindingCategoryCIReviewBot:
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
	act := item.ActionOrDefault()
	if sev == types.FindingSeverityInfo || act == types.ActionNoOp {
		return types.ChangeKindOther
	}
	if step == types.StepReview || step == types.StepTest || step == types.StepCI || item.Category == types.FindingCategoryCICheck {
		if sev == types.FindingSeverityError || sev == types.FindingSeverityWarning {
			if act == types.ActionAutoFix {
				return types.ChangeKindBug
			}
			// ask-user findings are design/intent unless a fix round actually
			// selected them; that selection is applied by the caller via Fixed.
			return types.ChangeKindOther
		}
	}
	return types.ChangeKindOther
}

func isCorrectnessFinding(step types.StepName, item types.Finding) bool {
	if item.ReviewScope == types.FindingReviewScopePipelineOwnedDelivery {
		return false
	}
	switch item.Category {
	case types.FindingCategoryDocumentation, types.FindingCategoryLint,
		types.FindingCategoryCIMergeConflict, types.FindingCategoryCITransient, types.FindingCategoryCIReviewBot:
		return false
	}
	sev := types.NormalizeFindingSeverity(item.Severity)
	if sev != types.FindingSeverityError && sev != types.FindingSeverityWarning {
		return false
	}
	switch step {
	case types.StepReview, types.StepTest, types.StepCI:
		return true
	}
	return item.Category == types.FindingCategoryCICheck
}

func selectedIDs(rounds []*db.StepRound) map[string]bool {
	out := make(map[string]bool)
	for _, round := range rounds {
		if round == nil || round.SelectedFindingIDs == nil {
			continue
		}
		var ids []string
		if json.Unmarshal([]byte(*round.SelectedFindingIDs), &ids) != nil {
			continue
		}
		for _, id := range ids {
			if id != "" {
				out[id] = true
			}
		}
	}
	return out
}

func fixedIDs(rounds []*db.StepRound, selected map[string]bool) map[string]bool {
	out := make(map[string]bool)
	if len(selected) == 0 {
		return out
	}
	sawFix := false
	for _, round := range rounds {
		if round != nil && round.IsFixRound() {
			sawFix = true
			break
		}
	}
	if !sawFix {
		return out
	}
	for id := range selected {
		out[id] = true
	}
	return out
}

func fingerprint(item types.Finding) string {
	norm := strings.ToLower(strings.Join([]string{
		strings.TrimSpace(item.File),
		itoa(item.Line),
		strings.Join(strings.Fields(strings.TrimSpace(item.Description)), " "),
	}, "|"))
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])[:16]
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	n := v
	if n < 0 {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if v < 0 {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

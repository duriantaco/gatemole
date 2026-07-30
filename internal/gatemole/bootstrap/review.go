package bootstrap

import (
	"fmt"
	"sort"
	"strings"
)

const ReviewVersion = "gatemole.bootstrap_review.v0"

type ReviewOptions struct {
	Limit int
	All   bool
}

type ReviewResult struct {
	Version         string        `json:"version"`
	Repo            string        `json:"repo"`
	TotalDrafts     int           `json:"total_drafts"`
	DisplayedDrafts int           `json:"displayed_drafts"`
	Drafts          []ReviewDraft `json:"drafts"`
}

type ReviewDraft struct {
	Component   string   `json:"component"`
	Risk        string   `json:"risk"`
	Why         string   `json:"why"`
	Signals     int      `json:"signals"`
	TestsFound  int      `json:"tests_found"`
	Obligations int      `json:"obligations"`
	Edit        string   `json:"edit"`
	Review      []string `json:"review"`
}

func BuildReview(result Result, opts ReviewOptions) ReviewResult {
	limit := opts.Limit
	if limit <= 0 {
		limit = 8
	}
	ranked := append([]Draft(nil), result.Drafts...)
	sort.Slice(ranked, func(i, j int) bool {
		leftRisk := reviewRiskRank(ranked[i].Risk)
		rightRisk := reviewRiskRank(ranked[j].Risk)
		if leftRisk != rightRisk {
			return leftRisk > rightRisk
		}
		if len(ranked[i].Signals) != len(ranked[j].Signals) {
			return len(ranked[i].Signals) > len(ranked[j].Signals)
		}
		leftTests := reviewTestCount(ranked[i])
		rightTests := reviewTestCount(ranked[j])
		if leftTests != rightTests {
			return leftTests > rightTests
		}
		return ranked[i].Component < ranked[j].Component
	})
	if !opts.All && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	review := ReviewResult{
		Version:     ReviewVersion,
		Repo:        result.Repo,
		TotalDrafts: len(result.Drafts),
		Drafts:      make([]ReviewDraft, 0, len(ranked)),
	}
	for _, draft := range ranked {
		review.Drafts = append(review.Drafts, ReviewDraft{
			Component:   draft.Component,
			Risk:        draft.Risk,
			Why:         reviewWhy(draft),
			Signals:     len(draft.Signals),
			TestsFound:  reviewTestCount(draft),
			Obligations: len(draft.Obligations),
			Edit:        draft.IntentPath,
			Review:      reviewChecklist(draft),
		})
	}
	review.DisplayedDrafts = len(review.Drafts)
	return review
}

func RenderReview(review ReviewResult) string {
	var b strings.Builder
	b.WriteString("Recommended contract drafts:\n")
	if len(review.Drafts) == 0 {
		b.WriteString("  none found\n")
		return b.String()
	}
	if review.DisplayedDrafts < review.TotalDrafts {
		fmt.Fprintf(&b, "Showing %d of %d. Use --all to show every draft.\n", review.DisplayedDrafts, review.TotalDrafts)
	}
	b.WriteByte('\n')
	for _, draft := range review.Drafts {
		fmt.Fprintf(&b, "%s %s\n", reviewRiskLabel(draft.Risk), draft.Component)
		fmt.Fprintf(&b, "  why: %s\n", draft.Why)
		fmt.Fprintf(&b, "  tests found: %d\n", draft.TestsFound)
		fmt.Fprintf(&b, "  obligations: %d\n", draft.Obligations)
		fmt.Fprintf(&b, "  review: %s\n", strings.Join(draft.Review, ", "))
		fmt.Fprintf(&b, "  edit: %s\n\n", draft.Edit)
	}
	return b.String()
}

func reviewRiskLabel(risk string) string {
	if risk == "medium" {
		return "MED"
	}
	return strings.ToUpper(risk)
}

func reviewRiskRank(risk string) int {
	switch risk {
	case "critical":
		return 3
	case "high":
		return 2
	case "medium":
		return 1
	default:
		return 0
	}
}

func reviewTestCount(draft Draft) int {
	count := 0
	for _, signal := range draft.Signals {
		if signal.Type == "test" {
			count++
		}
	}
	return count
}

func reviewWhy(draft Draft) string {
	for _, signal := range draft.Signals {
		if signal.Type == "path" && signal.Risk != "" {
			return signal.File + " risk is " + signal.Risk
		}
	}
	for _, signal := range draft.Signals {
		if signal.Type == "test" && signal.File != "" {
			return "test coverage found in " + signal.File
		}
	}
	return "repository signals found"
}

func reviewChecklist(draft Draft) []string {
	items := []string{"owner", "risk"}
	if draft.Risk == "high" || draft.Risk == "critical" {
		items = append(items, "security obligations")
	} else {
		items = append(items, "behavior obligations")
	}
	if reviewTestCount(draft) > 0 {
		items = append(items, "required tests")
	}
	return items
}

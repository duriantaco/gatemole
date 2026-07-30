package bootstrap

import (
	"fmt"
	"sort"
	"strings"
)

func RenderText(result Result) string {
	var b strings.Builder
	if len(result.Drafts) == 0 {
		b.WriteString("Detected 0 contract drafts.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "Detected %d contract drafts:\n\n", len(result.Drafts))
	for _, draft := range result.Drafts {
		fmt.Fprintf(&b, "%s %s\n", strings.ToUpper(draft.Risk), draft.Component)
		b.WriteString("  paths:\n")
		for _, path := range draft.Paths {
			fmt.Fprintf(&b, "    %s\n", path)
		}
		b.WriteString("  signals:\n")
		for _, signal := range conciseSignals(draft.Signals) {
			if signal.Symbol != "" {
				fmt.Fprintf(&b, "    %s: %s::%s\n", signal.Type, signal.File, signal.Symbol)
			} else if signal.Detail != "" {
				fmt.Fprintf(&b, "    %s: %s\n", signal.Type, signal.Detail)
			} else {
				fmt.Fprintf(&b, "    %s: %s\n", signal.Type, signal.File)
			}
		}
		b.WriteString("  drafted obligations:\n")
		for _, obligation := range draft.Obligations {
			shortID := strings.TrimPrefix(obligation.ID, draft.Component+".")
			fmt.Fprintf(&b, "    %s\n", shortID)
			if obligation.Generated.Source.File != "" {
				fmt.Fprintf(&b, "      from %s", obligation.Generated.Source.File)
				if obligation.Generated.Source.Symbol != "" {
					fmt.Fprintf(&b, "::%s", obligation.Generated.Source.Symbol)
				}
				b.WriteByte('\n')
			}
		}
		b.WriteByte('\n')
	}
	if len(result.Wrote) > 0 {
		b.WriteString("Wrote:\n")
		for _, path := range result.Wrote {
			fmt.Fprintf(&b, "  %s\n", path)
		}
	}
	if result.Check && result.NeedsWrite {
		b.WriteString("Bootstrap check failed: generated drafts are not up to date.\n")
	}
	return b.String()
}

func conciseSignals(signals []Signal) []Signal {
	filtered := make([]Signal, 0, len(signals))
	for _, signal := range signals {
		if signal.Type == "test" || signal.Type == "owner" || signal.Type == "openapi" || signal.Type == "ci" {
			filtered = append(filtered, signal)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Type == filtered[j].Type {
			if filtered[i].File == filtered[j].File {
				return filtered[i].Symbol < filtered[j].Symbol
			}
			return filtered[i].File < filtered[j].File
		}
		return filtered[i].Type < filtered[j].Type
	})
	if len(filtered) > 5 {
		return filtered[:5]
	}
	return filtered
}

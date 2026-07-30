package bootstrap

import (
	"path/filepath"
	"sort"
	"strings"
)

func Run(repo string, opts Options) (Result, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return Result{}, err
	}
	signals, err := scan(absRepo, opts.Aggressive)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		Version: Version,
		Repo:    absRepo,
		Mode:    modeName(opts),
		Check:   opts.Check,
		Drafts:  draftsFromSignals(signals, opts),
	}
	for i := range result.Drafts {
		result.Drafts[i].IntentPath = filepath.ToSlash(filepath.Join(".vouch", "intents", result.Drafts[i].Component+".yaml"))
	}
	result.NeedsWrite = needsWrite(absRepo, result)
	if !opts.DryRun && !opts.Check {
		wrote, err := writeResult(absRepo, result)
		if err != nil {
			return Result{}, err
		}
		result.Wrote = wrote
		result.ReportPath = filepath.ToSlash(filepath.Join(".vouch", "build", "bootstrap-report.json"))
		result.NeedsWrite = false
	}
	return result, nil
}

func draftsFromSignals(signals []Signal, opts Options) []Draft {
	grouped := make(map[string][]Signal)
	for _, signal := range signals {
		component := componentForSignal(signal)
		if component == "" || component == "repo" {
			continue
		}
		grouped[component] = append(grouped[component], signal)
	}
	drafts := make([]Draft, 0, len(grouped))
	for _, component := range sortedKeys(grouped) {
		signals := grouped[component]
		if !opts.Aggressive && !hasSignalType(signals, "test") {
			continue
		}
		draft := Draft{
			Component: component,
			Owner:     ownerFromSignals(signals),
			Risk:      riskFromSignals(component, signals),
			Paths:     pathsFromSignals(component, signals),
			Signals:   signals,
		}
		draft.Obligations = obligationsForDraft(draft, opts)
		if len(draft.Obligations) == 0 {
			continue
		}
		drafts = append(drafts, draft)
	}
	return drafts
}

func obligationsForDraft(draft Draft, opts Options) []Obligation {
	var obligations []Obligation
	limit := 2
	if opts.Aggressive {
		limit = 5
	}
	for _, signal := range draft.Signals {
		if signal.Type != "test" || signal.Symbol == "" {
			continue
		}
		label := testLabel(signal.Symbol)
		obligations = append(obligations, Obligation{
			ID:          draft.Component + ".required_test." + slug(label),
			Kind:        "required_test",
			Description: label,
			Generated:   generated(signal),
		})
		if len(obligations) >= limit {
			break
		}
	}
	if draft.Risk == "high" && len(obligations) > 0 {
		source := firstSource(draft.Signals)
		description := "security-sensitive changes require explicit evidence"
		obligations = append(obligations, Obligation{
			ID:          draft.Component + ".security." + slug(description),
			Kind:        "security",
			Description: description,
			Generated:   generated(source),
		})
	}
	return uniqueObligations(obligations)
}

func generated(signal Signal) Generated {
	return Generated{
		By:         "gatemole.bootstrap",
		Mode:       "deterministic",
		Confidence: "high",
		Source: SignalSource{
			Type:   signal.Type,
			File:   signal.File,
			Symbol: signal.Symbol,
			Detail: signal.Detail,
		},
	}
}

func componentForSignal(signal Signal) string {
	path := signal.File
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	if len(parts) == 0 {
		return ""
	}
	if parts[0] == "tests" {
		return componentFromTestPath(parts)
	}
	return componentFromPath(parts)
}

func componentFromTestPath(parts []string) string {
	if len(parts) == 1 {
		return strings.TrimSuffix(strings.TrimPrefix(parts[0], "test_"), filepath.Ext(parts[0]))
	}
	base := strings.TrimSuffix(parts[len(parts)-1], filepath.Ext(parts[len(parts)-1]))
	base = strings.TrimPrefix(base, "test_")
	componentParts := append([]string{}, parts[1:len(parts)-1]...)
	if base != "" && base != "test" {
		componentParts = append(componentParts, base)
	}
	return cleanComponent(componentParts)
}

func componentFromPath(parts []string) string {
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "internal", "src", "pkg", "lib", "app":
			continue
		default:
			filtered = append(filtered, part)
		}
	}
	if len(filtered) == 0 {
		return ""
	}
	last := filtered[len(filtered)-1]
	filtered[len(filtered)-1] = strings.TrimSuffix(last, filepath.Ext(last))
	return cleanComponent(filtered)
}

func cleanComponent(parts []string) string {
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		part = slug(part)
		if part != "" && part != "test" {
			cleaned = append(cleaned, part)
		}
	}
	if len(cleaned) == 0 {
		return ""
	}
	if len(cleaned) > 2 {
		cleaned = cleaned[len(cleaned)-2:]
	}
	return strings.Join(cleaned, ".")
}

func ownerFromSignals(signals []Signal) string {
	for _, signal := range signals {
		if signal.Type == "owner" && signal.Detail != "" {
			return signal.Detail
		}
	}
	return "unowned"
}

func riskFromSignals(component string, signals []Signal) string {
	risks := []string{riskFor(component)}
	for _, signal := range signals {
		risks = append(risks, signal.Risk)
	}
	return maxRisk(risks...)
}

func pathsFromSignals(component string, signals []Signal) []string {
	paths := []string{}
	for _, signal := range signals {
		if signal.File == "" {
			continue
		}
		if signal.Type == "test" {
			paths = append(paths, signal.File)
			paths = append(paths, guessedOwnedPath(component, signal.File))
		}
		if signal.Type == "path" {
			paths = append(paths, signal.File)
		}
	}
	return unique(paths)
}

func hasSignalType(signals []Signal, signalType string) bool {
	for _, signal := range signals {
		if signal.Type == signalType {
			return true
		}
	}
	return false
}

func guessedOwnedPath(component string, testFile string) string {
	parts := strings.Split(component, ".")
	if len(parts) == 0 {
		return testFile
	}
	return filepath.ToSlash(filepath.Join("**", filepath.Join(parts...), "*"))
}

func uniqueObligations(values []Obligation) []Obligation {
	seen := make(map[string]bool, len(values))
	var out []Obligation
	for _, value := range values {
		if value.ID == "" || seen[value.ID] {
			continue
		}
		seen[value.ID] = true
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func firstSource(signals []Signal) Signal {
	if len(signals) == 0 {
		return Signal{Type: "path"}
	}
	return signals[0]
}

func modeName(opts Options) string {
	if opts.Aggressive {
		return "aggressive"
	}
	return "conservative"
}

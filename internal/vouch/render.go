package vouch

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

func RenderVerification(evidence Evidence) string {
	if len(evidence.Findings) == 0 {
		return "Evidence verification: pass\n"
	}
	var b strings.Builder
	b.WriteString("Evidence verification:\n")
	for _, finding := range evidence.Findings {
		fmt.Fprintf(&b, "- [%s] %s/%s: %s\n", strings.ToUpper(finding.Decision), finding.Verifier, finding.Severity, finding.Claim)
		fmt.Fprintf(&b, "  evidence: %s\n", finding.Evidence)
		if finding.RequiredFix != "" {
			fmt.Fprintf(&b, "  required fix: %s\n", finding.RequiredFix)
		}
	}
	return b.String()
}

func RenderGate(evidence Evidence) string {
	return RenderGateWithOptions(evidence, GateRenderOptions{})
}

type GateRenderOptions struct {
	Explain bool
}

func RenderGateWithOptions(evidence Evidence, opts GateRenderOptions) string {
	covered, total := obligationCoverageCounts(evidence)
	var b strings.Builder
	b.WriteString(gateDecisionTitle(evidence.Decision))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "Release decision: %s\n", evidence.Decision)
	fmt.Fprintf(&b, "Obligations: %d/%d covered\n", covered, total)
	if evidence.PolicyResult.FiredPolicyRule != "" {
		fmt.Fprintf(&b, "Policy rule: %s\n", evidence.PolicyResult.FiredPolicyRule)
	}

	b.WriteString("\nCovered:\n")
	writeKindCounts(&b, obligationKindCounts(evidence.CoveredObligations), []string{
		string(ObligationRequiredTest),
		string(ObligationBehavior),
		string(ObligationSecurity),
		string(ObligationRuntimeSignal),
		string(ObligationRollback),
	})

	b.WriteString("\nMissing:\n")
	writeKindCounts(&b, evidenceKindCounts(evidence.MissingObligations), []string{
		string(EvidenceBehaviorTrace),
		string(EvidenceSecurityCheck),
		string(EvidenceRuntimeMetric),
		string(EvidenceRollbackPlan),
		string(EvidenceTestCoverage),
		string(EvidenceVerifierOutput),
	})

	why := gateWhyLines(evidence)
	if len(why) > 0 {
		b.WriteString("\nWhy:\n")
		for _, line := range why {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}

	next := gateNextSteps(evidence)
	if len(next) > 0 {
		b.WriteString("\nNext:\n")
		for i, step := range next {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, step)
		}
	}

	if opts.Explain {
		b.WriteString("\nEvidence types:\n")
		b.WriteString("  test_coverage: JUnit or mapped test results for required-test obligations\n")
		b.WriteString("  behavior_trace: evidence that user-visible behavior still matches the contract\n")
		b.WriteString("  security_check: security review, scanner, or verifier evidence for security obligations\n")
		b.WriteString("  runtime_metric: metric or alert evidence for runtime obligations\n")
		b.WriteString("  rollback_plan: rollback strategy or artifact for release recovery\n")
	}
	return b.String()
}

func RenderGateVerbose(evidence Evidence) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Release decision: %s\n", evidence.Decision)
	for _, reason := range evidence.Reasons {
		fmt.Fprintf(&b, "- %s\n", reason)
	}
	for _, specID := range sortedRenderKeys(evidence.RequiredObligations) {
		fmt.Fprintf(&b, "\nComponent:\n  %s\n", specID)
		if len(evidence.CoveredObligations[specID]) > 0 {
			b.WriteString("Covered:\n")
			for _, obligation := range evidence.CoveredObligations[specID] {
				fmt.Fprintf(&b, "  - %s\n", renderShortObligationID(specID, obligation.ID))
			}
		}
		if len(evidence.MissingObligations[specID]) > 0 {
			b.WriteString("Missing:\n")
			for _, obligation := range evidence.MissingObligations[specID] {
				fmt.Fprintf(&b, "  - %s\n", renderShortObligationID(specID, obligation.ID))
				fmt.Fprintf(&b, "    accepted evidence: %s\n", obligation.RequiredEvidence)
			}
		}
	}
	return b.String()
}

func gateDecisionTitle(decision string) string {
	switch decision {
	case "block":
		return "BLOCKED"
	case "auto_merge":
		return "AUTO_MERGE"
	case "human_escalation":
		return "HUMAN_ESCALATION"
	case "canary":
		return "CANARY"
	default:
		return strings.ToUpper(decision)
	}
}

func obligationKindCounts(items map[string][]Obligation) map[string]int {
	counts := map[string]int{}
	for _, specID := range sortedRenderKeys(items) {
		for _, obligation := range items[specID] {
			counts[string(obligation.Kind)]++
		}
	}
	return counts
}

func evidenceKindCounts(items map[string][]Obligation) map[string]int {
	counts := map[string]int{}
	for _, specID := range sortedRenderKeys(items) {
		for _, obligation := range items[specID] {
			counts[string(obligation.RequiredEvidence)]++
		}
	}
	return counts
}

func writeKindCounts(b *strings.Builder, counts map[string]int, order []string) {
	wrote := false
	for _, key := range order {
		count := counts[key]
		if count == 0 {
			continue
		}
		fmt.Fprintf(b, "  %s: %d\n", key, count)
		wrote = true
	}
	for _, key := range sortedStringKeys(counts) {
		if containsString(order, key) || counts[key] == 0 {
			continue
		}
		fmt.Fprintf(b, "  %s: %d\n", key, counts[key])
		wrote = true
	}
	if !wrote {
		b.WriteString("  none\n")
	}
}

func gateWhyLines(evidence Evidence) []string {
	var lines []string
	lines = append(lines, evidence.Reasons...)
	if hasCoveredEvidenceKind(evidence, EvidenceTestCoverage) && hasMissingNonTestEvidence(evidence) {
		lines = append(lines, "tests cover required-test obligations only")
		lines = append(lines, "missing behavior/security/runtime/rollback evidence can still block release")
	}
	if len(evidence.InvalidEvidence) > 0 {
		lines = append(lines, "one or more evidence artifacts were invalid")
	}
	return uniqueStrings(lines)
}

func gateNextSteps(evidence Evidence) []string {
	var steps []string
	if specID := firstMissingSpec(evidence); specID != "" {
		steps = append(steps, "Review "+filepath.ToSlash(filepath.Join(".vouch", "intents", specID+".yaml")))
	}
	for _, kind := range firstMissingEvidenceKinds(evidence, 2) {
		steps = append(steps, "Attach "+kind+" evidence")
	}
	if len(steps) > 0 {
		steps = append(steps, "Re-run vouch gate")
	}
	return steps
}

func firstMissingSpec(evidence Evidence) string {
	keys := sortedRenderKeys(evidence.MissingObligations)
	for _, key := range keys {
		if len(evidence.MissingObligations[key]) > 0 {
			return key
		}
	}
	return ""
}

func firstMissingEvidenceKinds(evidence Evidence, limit int) []string {
	counts := evidenceKindCounts(evidence.MissingObligations)
	order := []string{
		string(EvidenceBehaviorTrace),
		string(EvidenceSecurityCheck),
		string(EvidenceRuntimeMetric),
		string(EvidenceRollbackPlan),
		string(EvidenceTestCoverage),
		string(EvidenceVerifierOutput),
	}
	var kinds []string
	for _, kind := range order {
		if counts[kind] == 0 {
			continue
		}
		kinds = append(kinds, kind)
		if len(kinds) >= limit {
			return kinds
		}
	}
	return kinds
}

func hasCoveredEvidenceKind(evidence Evidence, kind EvidenceKind) bool {
	for _, specID := range sortedRenderKeys(evidence.CoveredObligations) {
		for _, obligation := range evidence.CoveredObligations[specID] {
			if obligation.RequiredEvidence == kind {
				return true
			}
		}
	}
	return false
}

func hasMissingNonTestEvidence(evidence Evidence) bool {
	for _, specID := range sortedRenderKeys(evidence.MissingObligations) {
		for _, obligation := range evidence.MissingObligations[specID] {
			if obligation.RequiredEvidence != EvidenceTestCoverage {
				return true
			}
		}
	}
	return false
}

func RenderGitHubSummary(evidence Evidence) string {
	covered, total := obligationCoverageCounts(evidence)
	var b strings.Builder
	b.WriteString("# Vouch Gate\n\n")
	b.WriteString("| Field | Value |\n")
	b.WriteString("| --- | --- |\n")
	fmt.Fprintf(&b, "| Decision | `%s` |\n", markdownCell(evidence.Decision))
	fmt.Fprintf(&b, "| Risk | `%s` |\n", markdownCell(string(evidence.Manifest.Change.Risk)))
	fmt.Fprintf(&b, "| Obligations | `%d/%d covered` |\n", covered, total)
	fmt.Fprintf(&b, "| Policy | `%s` |\n", markdownCell(evidence.PolicyPath))
	b.WriteByte('\n')

	if len(evidence.Reasons) > 0 {
		b.WriteString("## Reasons\n\n")
		for _, reason := range evidence.Reasons {
			fmt.Fprintf(&b, "- %s\n", reason)
		}
		b.WriteByte('\n')
	}

	b.WriteString("## Components\n\n")
	if len(evidence.RequiredObligations) == 0 {
		b.WriteString("No compiled obligations were required for this gate.\n\n")
	} else {
		for _, specID := range sortedRenderKeys(evidence.RequiredObligations) {
			fmt.Fprintf(&b, "### `%s`\n\n", markdownCell(specID))
			b.WriteString("| Status | Obligation | Accepted Evidence |\n")
			b.WriteString("| --- | --- | --- |\n")
			for _, obligation := range evidence.CoveredObligations[specID] {
				fmt.Fprintf(&b, "| Covered | `%s` | `%s` |\n", markdownCell(renderShortObligationID(specID, obligation.ID)), obligation.RequiredEvidence)
			}
			for _, obligation := range evidence.MissingObligations[specID] {
				fmt.Fprintf(&b, "| Missing | `%s` | `%s` |\n", markdownCell(renderShortObligationID(specID, obligation.ID)), obligation.RequiredEvidence)
			}
			b.WriteByte('\n')
		}
	}

	if len(evidence.Findings) > 0 {
		b.WriteString("## Findings\n\n")
		b.WriteString("| Decision | Verifier | Claim | Required Fix |\n")
		b.WriteString("| --- | --- | --- | --- |\n")
		for _, finding := range evidence.Findings {
			fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n", markdownCell(finding.Decision), markdownCell(finding.Verifier), markdownCell(finding.Claim), markdownCell(finding.RequiredFix))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func GateResultFromEvidence(evidence Evidence) GateResult {
	return GateResult{
		Version:            "gatemole.gate_result.v0",
		Decision:           evidence.Decision,
		Reasons:            cloneStrings(evidence.Reasons),
		PolicyPath:         evidence.PolicyPath,
		Compilation:        evidence.Compilation,
		SpecErrors:         cloneStrings(evidence.SpecErrors),
		ManifestErrors:     cloneStrings(evidence.ManifestErrors),
		ArtifactResults:    cloneArtifactResults(evidence.ArtifactResults),
		InvalidEvidence:    cloneInvalidEvidence(evidence.InvalidEvidence),
		MissingObligations: obligationTextMap(evidence.MissingObligations),
		CoveredObligations: obligationTextMap(evidence.CoveredObligations),
		PolicyRulesFired:   cloneStrings(evidence.PolicyResult.RulesFired),
		FiredPolicyRule:    evidence.PolicyResult.FiredPolicyRule,
	}
}

func cloneStrings(values []string) []string {
	out := make([]string, 0, len(values))
	return append(out, values...)
}

func cloneArtifactResults(values []ArtifactResult) []ArtifactResult {
	out := make([]ArtifactResult, 0, len(values))
	return append(out, values...)
}

func cloneInvalidEvidence(values []InvalidEvidence) []InvalidEvidence {
	out := make([]InvalidEvidence, 0, len(values))
	return append(out, values...)
}

func RenderGateResultJSON(evidence Evidence) (string, error) {
	data, err := json.MarshalIndent(GateResultFromEvidence(evidence), "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}

func RenderEvidence(evidence Evidence) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Vouch Evidence Report")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Repo: %s\n", evidence.Repo)
	fmt.Fprintf(&b, "Manifest: %s\n", evidence.ManifestPath)
	fmt.Fprintf(&b, "Task: %s - %s\n", evidence.Manifest.Task.ID, evidence.Manifest.Task.Summary)
	fmt.Fprintf(&b, "Risk: %s\n", evidence.Manifest.Change.Risk)
	fmt.Fprintf(&b, "Decision: %s\n", evidence.Decision)
	fmt.Fprintf(&b, "Compiled specs: %d/%d (%d skipped), obligations: %d\n", evidence.Compilation.SpecsCompiled, evidence.Compilation.SpecsLoaded, evidence.Compilation.SpecsSkipped, evidence.Compilation.ObligationsBuilt)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Spec Coverage")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "IR obligations covered: %s\n", obligationCoverageLine(evidence))
	fmt.Fprintf(&b, "Required tests covered: %s\n", coverageLine(evidence.RequiredTests, evidence.CoveredTests))
	fmt.Fprintf(&b, "Security invariants covered: %s\n", coverageLine(evidence.RequiredSecurity, evidence.CoveredSecurity))
	for _, specID := range sortedRenderKeys(evidence.MissingObligations) {
		missing := evidence.MissingObligations[specID]
		for _, obligation := range missing {
			if obligation.Kind == ObligationRequiredTest || obligation.Kind == ObligationSecurity {
				continue
			}
			fmt.Fprintf(&b, "- Missing %s obligation for %s: %s\n", obligation.Kind, specID, obligation.Text)
		}
	}
	for _, specID := range sortedRenderKeys(evidence.MissingTests) {
		missing := evidence.MissingTests[specID]
		if len(missing) > 0 {
			fmt.Fprintf(&b, "- Missing tests for %s: %s\n", specID, strings.Join(missing, ", "))
		}
	}
	for _, specID := range sortedRenderKeys(evidence.MissingSecurity) {
		missing := evidence.MissingSecurity[specID]
		if len(missing) > 0 {
			fmt.Fprintf(&b, "- Missing security checks for %s: %s\n", specID, strings.Join(missing, ", "))
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Evidence Verification")
	fmt.Fprintln(&b)
	if len(evidence.Findings) == 0 {
		fmt.Fprintln(&b, "- pass")
	} else {
		for _, finding := range evidence.Findings {
			fmt.Fprintf(&b, "- %s %s/%s: %s\n", strings.ToUpper(finding.Decision), finding.Verifier, finding.Severity, finding.Claim)
			fmt.Fprintf(&b, "  Evidence: %s\n", finding.Evidence)
			if finding.RequiredFix != "" {
				fmt.Fprintf(&b, "  Required fix: %s\n", finding.RequiredFix)
			}
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Release Gate")
	fmt.Fprintln(&b)
	if len(evidence.Reasons) == 0 {
		fmt.Fprintln(&b, "- no release-gate reasons recorded")
	} else {
		for _, reason := range evidence.Reasons {
			fmt.Fprintf(&b, "- %s\n", reason)
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Runtime And Rollback")
	fmt.Fprintln(&b)
	if len(evidence.Manifest.Runtime.Metrics) > 0 {
		fmt.Fprintf(&b, "Watch: %s\n", strings.Join(evidence.Manifest.Runtime.Metrics, ", "))
	}
	if evidence.Manifest.Runtime.Canary.Enabled {
		fmt.Fprintf(&b, "Canary: enabled at %d percent\n", evidence.Manifest.Runtime.Canary.InitialPercent)
	}
	if evidence.Manifest.Rollback.Strategy != "" {
		fmt.Fprintf(&b, "Rollback: %s\n", evidence.Manifest.Rollback.Strategy)
	}
	if evidence.Manifest.Rollback.Flag != "" {
		fmt.Fprintf(&b, "Flag: %s\n", evidence.Manifest.Rollback.Flag)
	}
	if len(evidence.Manifest.Uncertainties) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Uncertainties")
		fmt.Fprintln(&b)
		for _, item := range evidence.Manifest.Uncertainties {
			fmt.Fprintf(&b, "- %s\n", item)
		}
	}
	return b.String()
}

func RenderJSON(evidence Evidence) (string, error) {
	data, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}

func coverageLine(required map[string][]string, covered map[string][]string) string {
	total := 0
	seen := 0
	for _, specID := range sortedRenderKeys(required) {
		items := required[specID]
		total += len(items)
		seen += len(covered[specID])
	}
	return fmt.Sprintf("%d/%d", seen, total)
}

func obligationCoverageLine(evidence Evidence) string {
	total := 0
	seen := 0
	for _, specID := range sortedRenderKeys(evidence.RequiredObligations) {
		obligations := evidence.RequiredObligations[specID]
		total += len(obligations)
		seen += len(evidence.CoveredObligations[specID])
	}
	return fmt.Sprintf("%d/%d", seen, total)
}

func sortedRenderKeys[V any](items map[string]V) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func obligationTextMap(items map[string][]Obligation) map[string][]string {
	out := make(map[string][]string, len(items))
	for _, specID := range sortedRenderKeys(items) {
		for _, obligation := range items[specID] {
			out[specID] = append(out[specID], obligation.ID)
		}
	}
	return out
}

func renderShortObligationID(specID string, obligationID string) string {
	return strings.TrimPrefix(obligationID, specID+".")
}

func obligationCoverageCounts(evidence Evidence) (int, int) {
	total := 0
	covered := 0
	for _, specID := range sortedRenderKeys(evidence.RequiredObligations) {
		total += len(evidence.RequiredObligations[specID])
		covered += len(evidence.CoveredObligations[specID])
	}
	return covered, total
}

func markdownCell(value string) string {
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

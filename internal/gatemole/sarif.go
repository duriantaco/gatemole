package gatemole

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const sarifVersion = "2.1.0"

type sarifLog struct {
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name  string      `json:"name"`
	Rules []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID                   string          `json:"id"`
	Name                 string          `json:"name"`
	ShortDescription     sarifMessage    `json:"shortDescription"`
	FullDescription      sarifMessage    `json:"fullDescription"`
	DefaultConfiguration sarifDefault    `json:"defaultConfiguration"`
	Properties           sarifProperties `json:"properties"`
}

type sarifDefault struct {
	Level string `json:"level"`
}

type sarifResult struct {
	RuleID     string          `json:"ruleId"`
	Level      string          `json:"level"`
	Message    sarifMessage    `json:"message"`
	Properties sarifProperties `json:"properties"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifProperties map[string]any

func sarifLooksLike(data []byte) bool {
	var header struct {
		Version string `json:"version"`
		Runs    []any  `json:"runs"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return false
	}
	return header.Version != "" && header.Runs != nil
}

func importSARIFReferences(data []byte, obligationIDs []string) ([]string, []string) {
	log, issues := parseSARIF(data)
	if len(issues) > 0 {
		return nil, issues
	}
	refs := sarifReferencedObligations(log, stringSet(obligationIDs))
	var covered []string
	for _, obligationID := range obligationIDs {
		if refs[obligationID] {
			covered = append(covered, obligationID)
		}
	}
	if len(covered) == 0 {
		issues = append(issues, "SARIF does not reference any expected obligation IDs")
	}
	return covered, issues
}

func importSARIFEvidence(data []byte, obligationIDs []string, index ObligationIndex) ([]string, []Finding, []string) {
	log, issues := parseSARIF(data)
	if len(issues) > 0 {
		return nil, nil, issues
	}

	obligations := make(map[string]bool, len(obligationIDs))
	for _, obligationID := range obligationIDs {
		if _, ok := index.ByID[obligationID]; !ok {
			issues = append(issues, fmt.Sprintf("unknown obligation %q", obligationID))
			continue
		}
		obligations[obligationID] = true
	}
	refs := sarifReferencedObligations(log, obligations)
	blocked := map[string]bool{}
	var findings []Finding

	for _, run := range log.Runs {
		rules := sarifRulesByID(run)
		ruleRefs := sarifRunRuleRefs(run, obligations)
		tool := run.Tool.Driver.Name
		if strings.TrimSpace(tool) == "" {
			tool = "sarif"
		}
		for _, result := range run.Results {
			resultRefs := sarifResultRefs(result, ruleRefs, obligations)
			if len(resultRefs) == 0 {
				continue
			}
			rule := rules[result.RuleID]
			severity := sarifSeverity(result, rule)
			if sarifSeverityRank(severity) < sarifSeverityRank("high") {
				continue
			}
			for _, obligationID := range resultRefs {
				blocked[obligationID] = true
			}
			findings = append(findings, Finding{
				Verifier:    "sarif",
				Severity:    severity,
				Decision:    "block",
				Claim:       sarifClaim(tool, result),
				Evidence:    sarifEvidence(result, resultRefs),
				RequiredFix: "fix the SARIF finding or attach passing security evidence",
				Obligations: resultRefs,
			})
		}
	}

	var covered []string
	for _, obligationID := range obligationIDs {
		if !obligations[obligationID] {
			continue
		}
		if !refs[obligationID] {
			issues = append(issues, fmt.Sprintf("SARIF does not reference obligation %s", obligationID))
			continue
		}
		if !blocked[obligationID] {
			covered = append(covered, obligationID)
		}
	}
	return covered, findings, issues
}

func parseSARIF(data []byte) (sarifLog, []string) {
	var log sarifLog
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&log); err != nil {
		return log, []string{fmt.Sprintf("cannot parse SARIF JSON: %v", err)}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return log, []string{"trailing JSON content after SARIF log"}
	}
	if log.Version != sarifVersion {
		return log, []string{fmt.Sprintf("SARIF version must be %s", sarifVersion)}
	}
	if len(log.Runs) == 0 {
		return log, []string{"SARIF log contains no runs"}
	}
	return log, nil
}

func sarifReferencedObligations(log sarifLog, obligations map[string]bool) map[string]bool {
	refs := map[string]bool{}
	for _, run := range log.Runs {
		ruleRefs := sarifRunRuleRefs(run, obligations)
		for _, ids := range ruleRefs {
			for id := range ids {
				refs[id] = true
			}
		}
		for _, result := range run.Results {
			for _, id := range sarifResultRefs(result, ruleRefs, obligations) {
				refs[id] = true
			}
		}
	}
	return refs
}

func sarifRunRuleRefs(run sarifRun, obligations map[string]bool) map[string]map[string]bool {
	refs := map[string]map[string]bool{}
	for _, rule := range run.Tool.Driver.Rules {
		ruleSet := sarifRuleRefs(rule, obligations)
		if len(ruleSet) > 0 {
			refs[rule.ID] = ruleSet
		}
	}
	return refs
}

func sarifRulesByID(run sarifRun) map[string]sarifRule {
	rules := make(map[string]sarifRule, len(run.Tool.Driver.Rules))
	for _, rule := range run.Tool.Driver.Rules {
		rules[rule.ID] = rule
	}
	return rules
}

func sarifRuleRefs(rule sarifRule, obligations map[string]bool) map[string]bool {
	refs := map[string]bool{}
	if obligations[rule.ID] {
		refs[rule.ID] = true
	}
	addSARIFPropertyRefs(refs, rule.Properties, obligations)
	return refs
}

func sarifResultRefs(result sarifResult, ruleRefs map[string]map[string]bool, obligations map[string]bool) []string {
	refs := map[string]bool{}
	if obligations[result.RuleID] {
		refs[result.RuleID] = true
	}
	for id := range ruleRefs[result.RuleID] {
		refs[id] = true
	}
	addSARIFPropertyRefs(refs, result.Properties, obligations)
	return sortedStringKeys(refs)
}

func addSARIFPropertyRefs(refs map[string]bool, value any, obligations map[string]bool) {
	switch typed := value.(type) {
	case string:
		if obligations[typed] {
			refs[typed] = true
		}
	case []any:
		for _, item := range typed {
			addSARIFPropertyRefs(refs, item, obligations)
		}
	case map[string]any:
		for _, item := range typed {
			addSARIFPropertyRefs(refs, item, obligations)
		}
	case sarifProperties:
		for _, item := range typed {
			addSARIFPropertyRefs(refs, item, obligations)
		}
	}
}

func sarifSeverity(result sarifResult, rule sarifRule) string {
	if severity := severityFromProperties(result.Properties); severity != "" {
		return severity
	}
	if severity := severityFromProperties(rule.Properties); severity != "" {
		return severity
	}
	level := strings.TrimSpace(result.Level)
	if level == "" {
		level = strings.TrimSpace(rule.DefaultConfiguration.Level)
	}
	switch level {
	case "error":
		return "high"
	case "warning":
		return "medium"
	case "note", "none":
		return "low"
	default:
		return "low"
	}
}

func severityFromProperties(properties sarifProperties) string {
	if severity := securitySeverity(properties["security-severity"]); severity != "" {
		return severity
	}
	for _, key := range []string{"severity", "problem.severity"} {
		if severity := normalizeSeverity(properties[key]); severity != "" {
			return severity
		}
	}
	return ""
}

func normalizeSeverity(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "critical":
		return "critical"
	case "high", "error":
		return "high"
	case "medium", "warning":
		return "medium"
	case "low", "note", "none":
		return "low"
	default:
		return ""
	}
}

func securitySeverity(value any) string {
	var score float64
	switch typed := value.(type) {
	case float64:
		score = typed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return ""
		}
		score = parsed
	default:
		return ""
	}
	switch {
	case score >= 9:
		return "critical"
	case score >= 7:
		return "high"
	case score >= 4:
		return "medium"
	default:
		return "low"
	}
}

func sarifClaim(tool string, result sarifResult) string {
	if strings.TrimSpace(result.RuleID) == "" {
		return tool + " reported a high-severity security finding"
	}
	return fmt.Sprintf("%s reported high-severity finding %s", tool, result.RuleID)
}

func sarifEvidence(result sarifResult, obligations []string) string {
	message := strings.TrimSpace(result.Message.Text)
	if message == "" {
		message = "SARIF result did not include a message"
	}
	return fmt.Sprintf("%s; obligations: %s", message, strings.Join(obligations, ", "))
}

func sarifSeverityRank(severity string) int {
	switch severity {
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

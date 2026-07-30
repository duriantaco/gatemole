package gatemole

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type IntentParser interface {
	ParseIntent(path string) (IntentAST, []Diagnostic, error)
}

type YAMLNodeIntentParser struct{}

func ParseIntentASTFile(path string) (IntentAST, []Diagnostic, error) {
	return YAMLNodeIntentParser{}.ParseIntent(path)
}

func (YAMLNodeIntentParser) ParseIntent(path string) (IntentAST, []Diagnostic, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return IntentAST{}, nil, err
	}

	ast := IntentAST{
		Version:     ASTSchemaVersion,
		File:        path,
		Nodes:       []ASTNode{},
		Diagnostics: []Diagnostic{},
	}
	var diagnostics []Diagnostic
	seenTopLevel := map[string]SourceSpan{}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		diagnostics = append(diagnostics, diagnostic("error", "intent.yaml_parse_error", err.Error(), "", SourceSpan{File: path}))
		ast.Diagnostics = diagnostics
		return ast, diagnostics, nil
	}
	if len(document.Content) == 0 {
		diagnostics = append(diagnostics, diagnostic("error", "intent.empty_document", "intent document must be a mapping", "", SourceSpan{File: path}))
		ast.Diagnostics = diagnostics
		return ast, diagnostics, nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		diagnostics = append(diagnostics, diagnostic("error", "intent.expected_mapping", "intent document must be a mapping", "", nodeSpan(path, root)))
		ast.Diagnostics = diagnostics
		return ast, diagnostics, nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		keyNode := root.Content[i]
		valueNode := root.Content[i+1]
		span := nodeSpan(path, keyNode)
		if keyNode.Kind != yaml.ScalarNode || strings.TrimSpace(keyNode.Value) == "" {
			diagnostics = append(diagnostics, diagnostic("error", "intent.empty_key", "key must be a non-empty scalar", "", span))
			continue
		}
		key := strings.TrimSpace(keyNode.Value)
		node, nodeDiagnostics, ok := parseIntentNode(path, key, valueNode, span, seenTopLevel)
		diagnostics = append(diagnostics, nodeDiagnostics...)
		if ok {
			ast.Nodes = append(ast.Nodes, node)
		}
	}
	diagnostics = append(diagnostics, ValidateIntentAST(ast)...)
	ast.Diagnostics = diagnostics
	return ast, diagnostics, nil
}

func parseIntentNode(path string, key string, valueNode *yaml.Node, span SourceSpan, seenTopLevel map[string]SourceSpan) (ASTNode, []Diagnostic, bool) {
	var diagnostics []Diagnostic
	if !supportedIntentKey(key) {
		diagnostics = append(diagnostics, diagnostic("error", "intent.unsupported_key", fmt.Sprintf("unsupported key %q", key), key, span))
		return ASTNode{}, diagnostics, false
	}
	if previous, exists := seenTopLevel[key]; exists {
		diagnostics = append(diagnostics, diagnostic("error", "intent.duplicate_key", fmt.Sprintf("duplicate key %q first declared at line %d", key, previous.Line), key, span))
	}
	seenTopLevel[key] = span
	node := ASTNode{Key: key, Span: span}
	switch {
	case supportedScalarIntentKey(key):
		node.Kind = "scalar"
		if !yamlScalarLike(valueNode) {
			diagnostics = append(diagnostics, diagnostic("error", "intent.expected_scalar", fmt.Sprintf("key %q must be a scalar value", key), key, nodeSpan(path, valueNode)))
			return node, diagnostics, true
		}
		node.Value = valueNode.Value
	case supportsListSection(key):
		node.Kind = "section"
		if valueNode.Kind != yaml.SequenceNode {
			diagnostics = append(diagnostics, diagnostic("error", "intent.expected_list", fmt.Sprintf("section %q must be a list", key), key, nodeSpan(path, valueNode)))
			return node, diagnostics, true
		}
		node.Values, diagnostics = parseIntentListValues(path, key, valueNode, diagnostics)
	case key == "rollback":
		node.Kind = "section"
		if valueNode.Kind != yaml.MappingNode {
			diagnostics = append(diagnostics, diagnostic("error", "intent.expected_mapping", "rollback must be a mapping", "rollback", nodeSpan(path, valueNode)))
			return node, diagnostics, true
		}
		node.Children, diagnostics = parseRollbackChildren(path, valueNode, diagnostics)
	}
	return node, diagnostics, true
}

func parseIntentListValues(path string, sectionKey string, valueNode *yaml.Node, diagnostics []Diagnostic) ([]ASTValue, []Diagnostic) {
	values := make([]ASTValue, 0, len(valueNode.Content))
	seen := map[string]SourceSpan{}
	for _, item := range valueNode.Content {
		span := nodeSpan(path, item)
		if !yamlScalarLike(item) {
			diagnostics = append(diagnostics, diagnostic("error", "intent.expected_list_scalar", fmt.Sprintf("section %q list items must be scalar values", sectionKey), sectionKey, span))
			continue
		}
		value := strings.TrimSpace(item.Value)
		if value == "" {
			diagnostics = append(diagnostics, diagnostic("error", "intent.empty_list_item", "list item must be non-empty", sectionKey, span))
			continue
		}
		if previous, exists := seen[value]; exists {
			diagnostics = append(diagnostics, diagnostic("error", "intent.duplicate_list_item", fmt.Sprintf("duplicate value %q first declared at line %d", value, previous.Line), sectionKey, span))
		}
		seen[value] = span
		values = append(values, ASTValue{Value: value, Span: span})
	}
	return values, diagnostics
}

func parseRollbackChildren(path string, rollbackNode *yaml.Node, diagnostics []Diagnostic) ([]ASTNode, []Diagnostic) {
	children := []ASTNode{}
	seen := map[string]SourceSpan{}
	for i := 0; i+1 < len(rollbackNode.Content); i += 2 {
		keyNode := rollbackNode.Content[i]
		valueNode := rollbackNode.Content[i+1]
		span := nodeSpan(path, keyNode)
		if keyNode.Kind != yaml.ScalarNode || strings.TrimSpace(keyNode.Value) == "" {
			diagnostics = append(diagnostics, diagnostic("error", "intent.empty_rollback_key", "rollback key must be a non-empty scalar", "rollback", span))
			continue
		}
		key := strings.TrimSpace(keyNode.Value)
		childPath := "rollback." + key
		if key != "strategy" && key != "flag" {
			diagnostics = append(diagnostics, diagnostic("error", "intent.unsupported_rollback_key", fmt.Sprintf("unsupported rollback key %q", key), childPath, span))
			continue
		}
		if previous, exists := seen[key]; exists {
			diagnostics = append(diagnostics, diagnostic("error", "intent.duplicate_rollback_key", fmt.Sprintf("duplicate rollback key %q first declared at line %d", key, previous.Line), childPath, span))
		}
		seen[key] = span
		child := ASTNode{Kind: "scalar", Key: key, Span: span}
		if !yamlScalarLike(valueNode) {
			diagnostics = append(diagnostics, diagnostic("error", "intent.expected_scalar", fmt.Sprintf("rollback key %q must be a scalar value", key), childPath, nodeSpan(path, valueNode)))
		} else {
			child.Value = valueNode.Value
		}
		children = append(children, child)
	}
	return children, diagnostics
}

func WriteIntentASTFile(intentPath string, outPath string) (IntentAST, error) {
	ast, diagnostics, err := ParseIntentASTFile(intentPath)
	if err != nil {
		return IntentAST{}, err
	}
	if err := writeJSONFile(outPath, ast); err != nil {
		return IntentAST{}, err
	}
	if HasErrorDiagnostics(diagnostics) {
		return ast, DiagnosticError{Diagnostics: diagnostics}
	}
	return ast, nil
}

func CompileIntentFile(intentPath string, outPath string) (Spec, error) {
	ast, diagnostics, err := ParseIntentASTFile(intentPath)
	if err != nil {
		return Spec{}, err
	}
	if HasErrorDiagnostics(diagnostics) {
		return Spec{}, DiagnosticError{Diagnostics: diagnostics}
	}
	typed, diagnostics := AnalyzeIntentAST(ast)
	if HasErrorDiagnostics(diagnostics) {
		return Spec{}, DiagnosticError{Diagnostics: diagnostics}
	}
	intent := typed.Intent()
	spec := SpecFromIntent(intent)
	specDiagnostics := stringDiagnostics("spec", ValidateSpec(spec))
	if HasErrorDiagnostics(specDiagnostics) {
		return Spec{}, DiagnosticError{Diagnostics: specDiagnostics}
	}
	if err := writeJSONFile(outPath, spec); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

func ParseIntentFile(path string) (Intent, error) {
	ast, diagnostics, err := ParseIntentASTFile(path)
	if err != nil {
		return Intent{}, err
	}
	if HasErrorDiagnostics(diagnostics) {
		return Intent{}, DiagnosticError{Diagnostics: diagnostics}
	}
	typed, diagnostics := AnalyzeIntentAST(ast)
	if HasErrorDiagnostics(diagnostics) {
		return Intent{}, DiagnosticError{Diagnostics: diagnostics}
	}
	return typed.Intent(), nil
}

func ValidateIntentAST(ast IntentAST) []Diagnostic {
	var diagnostics []Diagnostic
	allowedListSections := map[string]bool{
		"owned_paths":     true,
		"behavior":        true,
		"security":        true,
		"required_tests":  true,
		"runtime_metrics": true,
		"runtime_alerts":  true,
	}
	for _, node := range ast.Nodes {
		switch node.Kind {
		case "scalar":
			if !supportedScalarIntentKey(node.Key) {
				diagnostics = append(diagnostics, diagnostic("error", "intent.unsupported_scalar", fmt.Sprintf("unsupported scalar key %q", node.Key), node.Key, node.Span))
			}
		case "section":
			if node.Key == "rollback" {
				continue
			}
			if !allowedListSections[node.Key] {
				diagnostics = append(diagnostics, diagnostic("error", "intent.unsupported_section", fmt.Sprintf("unsupported section %q", node.Key), node.Key, node.Span))
			}
		default:
			diagnostics = append(diagnostics, diagnostic("error", "intent.unsupported_node_kind", fmt.Sprintf("unsupported AST node kind %q", node.Kind), node.Key, node.Span))
		}
	}
	return diagnostics
}

func IntentFromAST(ast IntentAST) (Intent, []Diagnostic) {
	typed, diagnostics := AnalyzeIntentAST(ast)
	return typed.Intent(), diagnostics
}

type SourceValue[T any] struct {
	Value T
	Span  SourceSpan
}

type TypedIntent struct {
	Version        SourceValue[string]
	Feature        SourceValue[string]
	Owner          SourceValue[string]
	OwnedPaths     []SourceValue[string]
	Risk           SourceValue[Risk]
	Goal           SourceValue[string]
	Behavior       []SourceValue[string]
	Security       []SourceValue[string]
	RequiredTests  []SourceValue[string]
	RuntimeMetrics []SourceValue[string]
	RuntimeAlerts  []SourceValue[string]
	Rollback       TypedRollback
	Spans          map[string]SourceSpan
}

type TypedRollback struct {
	Strategy SourceValue[string]
	Flag     SourceValue[string]
}

func AnalyzeIntentAST(ast IntentAST) (TypedIntent, []Diagnostic) {
	var typed TypedIntent
	var diagnostics []Diagnostic
	spans := map[string]SourceSpan{}
	typed.Spans = spans
	for _, node := range ast.Nodes {
		spans[node.Key] = node.Span
		switch node.Key {
		case "version":
			typed.Version = SourceValue[string]{Value: node.Value, Span: node.Span}
		case "feature":
			typed.Feature = SourceValue[string]{Value: node.Value, Span: node.Span}
		case "owner":
			typed.Owner = SourceValue[string]{Value: node.Value, Span: node.Span}
		case "owned_paths":
			typed.OwnedPaths = typedValues(node.Values)
		case "risk":
			typed.Risk = SourceValue[Risk]{Value: Risk(node.Value), Span: node.Span}
		case "goal":
			typed.Goal = SourceValue[string]{Value: node.Value, Span: node.Span}
		case "behavior":
			typed.Behavior = typedValues(node.Values)
		case "security":
			typed.Security = typedValues(node.Values)
		case "required_tests":
			typed.RequiredTests = typedValues(node.Values)
		case "runtime_metrics":
			typed.RuntimeMetrics = typedValues(node.Values)
		case "runtime_alerts":
			typed.RuntimeAlerts = typedValues(node.Values)
		case "rollback":
			for _, child := range node.Children {
				spans["rollback."+child.Key] = child.Span
				switch child.Key {
				case "strategy":
					typed.Rollback.Strategy = SourceValue[string]{Value: child.Value, Span: child.Span}
				case "flag":
					typed.Rollback.Flag = SourceValue[string]{Value: child.Value, Span: child.Span}
				}
			}
		}
	}
	diagnostics = append(diagnostics, ValidateTypedIntent(typed)...)
	return typed, diagnostics
}

func (typed TypedIntent) Intent() Intent {
	return Intent{
		Version:        typed.Version.Value,
		Feature:        typed.Feature.Value,
		Owner:          typed.Owner.Value,
		OwnedPaths:     sourceValues(typed.OwnedPaths),
		Risk:           typed.Risk.Value,
		Goal:           typed.Goal.Value,
		Behavior:       sourceValues(typed.Behavior),
		Security:       sourceValues(typed.Security),
		RequiredTests:  sourceValues(typed.RequiredTests),
		RuntimeMetrics: sourceValues(typed.RuntimeMetrics),
		RuntimeAlerts:  sourceValues(typed.RuntimeAlerts),
		Rollback: SpecRollback{
			Strategy: typed.Rollback.Strategy.Value,
			Flag:     typed.Rollback.Flag.Value,
		},
	}
}

func ValidateIntent(intent Intent) []string {
	var errors []string
	for _, diagnostic := range ValidateIntentDiagnostics(intent) {
		errors = append(errors, diagnostic.Message)
	}
	return errors
}

func ValidateIntentDiagnostics(intent Intent) []Diagnostic {
	return ValidateIntentDiagnosticsWithSpans(intent, map[string]SourceSpan{})
}

func ValidateIntentDiagnosticsWithSpans(intent Intent, spans map[string]SourceSpan) []Diagnostic {
	typed := TypedIntent{
		Feature:        SourceValue[string]{Value: intent.Feature, Span: spans["feature"]},
		Version:        SourceValue[string]{Value: intent.Version, Span: spans["version"]},
		Owner:          SourceValue[string]{Value: intent.Owner, Span: spans["owner"]},
		OwnedPaths:     sourceStringsWithSpan(intent.OwnedPaths, spans["owned_paths"]),
		Risk:           SourceValue[Risk]{Value: intent.Risk, Span: spans["risk"]},
		Goal:           SourceValue[string]{Value: intent.Goal, Span: spans["goal"]},
		Behavior:       sourceStringsWithSpan(intent.Behavior, spans["behavior"]),
		Security:       sourceStringsWithSpan(intent.Security, spans["security"]),
		RequiredTests:  sourceStringsWithSpan(intent.RequiredTests, spans["required_tests"]),
		RuntimeMetrics: sourceStringsWithSpan(intent.RuntimeMetrics, spans["runtime_metrics"]),
		RuntimeAlerts:  sourceStringsWithSpan(intent.RuntimeAlerts, spans["runtime_alerts"]),
		Rollback: TypedRollback{
			Strategy: SourceValue[string]{Value: intent.Rollback.Strategy, Span: spans["rollback.strategy"]},
			Flag:     SourceValue[string]{Value: intent.Rollback.Flag, Span: spans["rollback.flag"]},
		},
		Spans: spans,
	}
	return ValidateTypedIntent(typed)
}

func ValidateTypedIntent(intent TypedIntent) []Diagnostic {
	var diagnostics []Diagnostic
	if intent.Version.Value != "" && intent.Version.Value != IntentSchemaVersion {
		diagnostics = append(diagnostics, diagnostic("error", "intent.invalid_version", fmt.Sprintf("version must be %s", IntentSchemaVersion), "version", intent.span("version")))
	}
	if intent.Feature.Value == "" {
		diagnostics = append(diagnostics, diagnostic("error", "intent.required_feature", "feature is required", "feature", intent.span("feature")))
	}
	if intent.Owner.Value == "" {
		diagnostics = append(diagnostics, diagnostic("error", "intent.required_owner", "owner is required", "owner", intent.span("owner")))
	}
	diagnostics = append(diagnostics, validateOwnedPathValues("intent", "owned_paths", sourceValues(intent.OwnedPaths), intent.span("owned_paths"))...)
	if !validRisk(intent.Risk.Value) {
		diagnostics = append(diagnostics, diagnostic("error", "intent.invalid_risk", "risk must be one of low, medium, high, critical", "risk", intent.span("risk")))
	}
	if len(intent.Behavior) == 0 {
		diagnostics = append(diagnostics, diagnostic("error", "intent.required_behavior", "behavior must include at least one contract", "behavior", intent.span("behavior")))
	}
	if len(intent.Security) == 0 {
		diagnostics = append(diagnostics, diagnostic("error", "intent.required_security", "security must include at least one invariant", "security", intent.span("security")))
	}
	if len(intent.RequiredTests) == 0 {
		diagnostics = append(diagnostics, diagnostic("error", "intent.required_tests", "required_tests must include at least one test obligation", "required_tests", intent.span("required_tests")))
	}
	if len(intent.RuntimeMetrics) == 0 {
		diagnostics = append(diagnostics, diagnostic("error", "intent.required_runtime_metrics", "runtime_metrics must include at least one signal", "runtime_metrics", intent.span("runtime_metrics")))
	}
	if intent.Rollback.Strategy.Value == "" {
		diagnostics = append(diagnostics, diagnostic("error", "intent.required_rollback_strategy", "rollback.strategy is required", "rollback.strategy", intent.span("rollback.strategy")))
	}
	return diagnostics
}

func (intent TypedIntent) span(path string) SourceSpan {
	if intent.Spans == nil {
		return SourceSpan{}
	}
	return intent.Spans[path]
}

func SpecFromIntent(intent Intent) Spec {
	return Spec{
		Version:    SpecSchemaVersion,
		ID:         intent.Feature,
		Owner:      intent.Owner,
		OwnedPaths: append([]string(nil), intent.OwnedPaths...),
		Risk:       intent.Risk,
		Behavior:   intent.Behavior,
		Security:   intent.Security,
		Tests: SpecTests{
			Required: intent.RequiredTests,
		},
		Runtime: SpecRuntime{
			Metrics: intent.RuntimeMetrics,
			Alerts:  intent.RuntimeAlerts,
		},
		Rollback: intent.Rollback,
	}
}

func supportedIntentKey(key string) bool {
	return supportedScalarIntentKey(key) || supportsListSection(key) || key == "rollback"
}

func supportedScalarIntentKey(key string) bool {
	return key == "version" || key == "feature" || key == "owner" || key == "risk" || key == "goal"
}

func supportsListSection(key string) bool {
	return key == "owned_paths" || key == "behavior" || key == "security" || key == "required_tests" || key == "runtime_metrics" || key == "runtime_alerts"
}

func astValues(values []ASTValue) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value.Value)
	}
	return out
}

func typedValues(values []ASTValue) []SourceValue[string] {
	out := make([]SourceValue[string], 0, len(values))
	for _, value := range values {
		out = append(out, SourceValue[string]{Value: value.Value, Span: value.Span})
	}
	return out
}

func sourceValues(values []SourceValue[string]) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value.Value)
	}
	return out
}

func sourceStringsWithSpan(values []string, span SourceSpan) []SourceValue[string] {
	out := make([]SourceValue[string], 0, len(values))
	for _, value := range values {
		out = append(out, SourceValue[string]{Value: value, Span: span})
	}
	return out
}

func yamlScalarLike(node *yaml.Node) bool {
	return node != nil && (node.Kind == yaml.ScalarNode || node.Kind == 0)
}

func nodeSpan(path string, node *yaml.Node) SourceSpan {
	if node == nil {
		return SourceSpan{File: path}
	}
	return SourceSpan{File: path, Line: node.Line, Column: node.Column}
}

func diagnostic(severity string, code string, message string, path string, span SourceSpan) Diagnostic {
	return Diagnostic{
		Severity: severity,
		Code:     code,
		Message:  message,
		Path:     path,
		Span:     span,
	}
}

func firstNonSpace(value string) int {
	for i, char := range value {
		if char != ' ' && char != '\t' {
			return i
		}
	}
	return 0
}

func writeJSONFile(path string, value any) error {
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data.Bytes(), 0o644)
}

package vouch

type Risk string

const (
	SpecSchemaVersion       = "gatemole.spec.v0"
	ManifestSchemaVersion   = "gatemole.manifest.v0"
	EvidenceSchemaVersion   = "gatemole.evidence.v0"
	EvidenceManifestVersion = "gatemole.evidence_manifest.v0"
	EvidenceBundleVersion   = "gatemole.evidence_bundle.v0"
	IntentSchemaVersion     = "gatemole.intent.v0"
	ASTSchemaVersion        = "gatemole.ast.v0"
	IRSchemaVersion         = "gatemole.ir.v0"
	PlanSchemaVersion       = "gatemole.plan.v0"
	ConfigSchemaVersion     = "gatemole.config.v0"
	CompileResultVersion    = "gatemole.compile_result.v0"
	ObligationsIRVersion    = "gatemole.obligations_ir.v0"
	PlanBundleVersion       = "gatemole.verification_plan_bundle.v0"
	TestMapSchemaVersion    = "gatemole.test_map.v0"
	PolicySchemaVersion     = "gatemole.policy.v0"
	PolicyInputVersion      = "gatemole.policy_input.v0"
	PolicyResultVersion     = "gatemole.policy_result.v0"
	PolicySimulationVersion = "gatemole.policy_simulation.v0"
	VerifierPromptVersion   = "gatemole.verifier_prompt.v0"
	VerifierOutputVersion   = "gatemole.verifier_output.v0"
)

const (
	RiskLow      Risk = "low"
	RiskMedium   Risk = "medium"
	RiskHigh     Risk = "high"
	RiskCritical Risk = "critical"
)

var riskRank = map[Risk]int{
	RiskLow:      0,
	RiskMedium:   1,
	RiskHigh:     2,
	RiskCritical: 3,
}

type Spec struct {
	Version    string       `json:"version"`
	ID         string       `json:"id"`
	Owner      string       `json:"owner"`
	OwnedPaths []string     `json:"owned_paths,omitempty"`
	Risk       Risk         `json:"risk"`
	Behavior   []string     `json:"behavior"`
	Security   []string     `json:"security"`
	Tests      SpecTests    `json:"tests"`
	Runtime    SpecRuntime  `json:"runtime"`
	Rollback   SpecRollback `json:"rollback"`
}

type SpecTests struct {
	Required []string `json:"required"`
}

type SpecRuntime struct {
	Metrics []string `json:"metrics"`
	Alerts  []string `json:"alerts"`
}

type SpecRollback struct {
	Strategy string `json:"strategy"`
	Flag     string `json:"flag"`
}

type ObligationKind string

const (
	ObligationBehavior      ObligationKind = "behavior"
	ObligationSecurity      ObligationKind = "security"
	ObligationRequiredTest  ObligationKind = "required_test"
	ObligationRuntimeSignal ObligationKind = "runtime_signal"
	ObligationRollback      ObligationKind = "rollback"
)

type EvidenceKind string

const (
	EvidenceBehaviorTrace  EvidenceKind = "behavior_trace"
	EvidenceSecurityCheck  EvidenceKind = "security_check"
	EvidenceTestCoverage   EvidenceKind = "test_coverage"
	EvidenceRuntimeMetric  EvidenceKind = "runtime_metric"
	EvidenceRollbackPlan   EvidenceKind = "rollback_plan"
	EvidenceVerifierOutput EvidenceKind = "verifier_output"
)

type Intent struct {
	Version        string
	Feature        string
	Owner          string
	OwnedPaths     []string
	Risk           Risk
	Goal           string
	Behavior       []string
	Security       []string
	RequiredTests  []string
	RuntimeMetrics []string
	RuntimeAlerts  []string
	Rollback       SpecRollback
}

type SourceSpan struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

type Diagnostic struct {
	Severity string     `json:"severity"`
	Code     string     `json:"code"`
	Message  string     `json:"message"`
	Path     string     `json:"path,omitempty"`
	Span     SourceSpan `json:"span"`
}

type IntentAST struct {
	Version     string       `json:"version"`
	File        string       `json:"file"`
	Nodes       []ASTNode    `json:"nodes"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

type ASTNode struct {
	Kind     string     `json:"kind"`
	Key      string     `json:"key"`
	Value    string     `json:"value,omitempty"`
	Values   []ASTValue `json:"values,omitempty"`
	Children []ASTNode  `json:"children,omitempty"`
	Span     SourceSpan `json:"span"`
}

type ASTValue struct {
	Value string     `json:"value"`
	Span  SourceSpan `json:"span"`
}

type IR struct {
	Version        string       `json:"version"`
	Feature        string       `json:"feature"`
	Owner          string       `json:"owner"`
	Risk           Risk         `json:"risk"`
	Obligations    []Obligation `json:"obligations"`
	RequiredChecks []string     `json:"required_checks"`
	RuntimeSignals []string     `json:"runtime_signals"`
	Rollback       SpecRollback `json:"rollback"`
	ReleasePolicy  []string     `json:"release_policy"`
}

type VerificationPlan struct {
	Version        string       `json:"version"`
	Feature        string       `json:"feature"`
	Risk           Risk         `json:"risk"`
	SpecsTouched   []string     `json:"specs_touched"`
	Obligations    []Obligation `json:"obligations"`
	RequiredChecks []string     `json:"required_checks"`
	VerifierRoles  []string     `json:"verifier_roles"`
	RuntimeSignals []string     `json:"runtime_signals"`
	Rollback       SpecRollback `json:"rollback"`
	ReleasePolicy  []string     `json:"release_policy"`
	Diagnostics    []Diagnostic `json:"diagnostics"`
}

type VerifierPacket struct {
	Verifier       string       `json:"verifier"`
	Focus          string       `json:"focus"`
	PromptVersion  string       `json:"prompt_version"`
	OutputSchema   string       `json:"output_schema"`
	Obligations    []Obligation `json:"obligations"`
	RequiredOutput string       `json:"required_output"`
}

type TestObligationsArtifact struct {
	Version     string       `json:"version"`
	Feature     string       `json:"feature"`
	Obligations []Obligation `json:"obligations"`
}

type ReleasePolicyArtifact struct {
	Version        string   `json:"version"`
	Feature        string   `json:"feature"`
	Risk           Risk     `json:"risk"`
	ReleasePolicy  []string `json:"release_policy"`
	RequiredChecks []string `json:"required_checks"`
}

type Obligation struct {
	ID               string         `json:"id"`
	Kind             ObligationKind `json:"kind"`
	Text             string         `json:"text"`
	Risk             Risk           `json:"risk"`
	Severity         string         `json:"severity"`
	Source           string         `json:"source"`
	RequiredEvidence EvidenceKind   `json:"required_evidence"`
	Generated        *GeneratedInfo `json:"generated,omitempty"`
}

type GeneratedInfo struct {
	By         string          `json:"by"`
	Mode       string          `json:"mode"`
	Confidence string          `json:"confidence"`
	Source     GeneratedSource `json:"source"`
}

type GeneratedSource struct {
	Type   string `json:"type"`
	File   string `json:"file,omitempty"`
	Symbol string `json:"symbol,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type Manifest struct {
	Version       string           `json:"version"`
	Task          Task             `json:"task"`
	Change        Change           `json:"change"`
	Agent         Agent            `json:"agent"`
	Verification  Verification     `json:"verification"`
	Runtime       ManifestRuntime  `json:"runtime"`
	Rollback      ManifestRollback `json:"rollback"`
	Uncertainties []string         `json:"uncertainties"`
}

type Task struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

type Change struct {
	Risk             Risk     `json:"risk"`
	SpecsTouched     []string `json:"specs_touched"`
	BehaviorChanged  bool     `json:"behavior_changed"`
	MigrationChanged bool     `json:"migration_changed"`
	ExternalEffects  []string `json:"external_effects"`
	ChangedFiles     []string `json:"changed_files"`
}

type Agent struct {
	Name             string `json:"name"`
	RunID            string `json:"run_id"`
	Model            string `json:"model"`
	RunnerIdentity   string `json:"runner_identity,omitempty"`
	RunnerOIDCIssuer string `json:"runner_oidc_issuer,omitempty"`
}

type Verification struct {
	CoversBehavior []string           `json:"covers_behavior"`
	Commands       []string           `json:"commands"`
	TestsAdded     []string           `json:"tests_added"`
	CoversTests    []string           `json:"covers_tests"`
	CoversSecurity []string           `json:"covers_security"`
	TestResults    TestResults        `json:"test_results"`
	Artifacts      []EvidenceArtifact `json:"artifacts"`
}

type EvidenceArtifact struct {
	ID               string       `json:"id"`
	Kind             EvidenceKind `json:"kind"`
	Producer         string       `json:"producer,omitempty"`
	Command          string       `json:"command,omitempty"`
	Path             string       `json:"path,omitempty"`
	SHA256           string       `json:"sha256,omitempty"`
	EvidenceBundle   string       `json:"evidence_bundle,omitempty"`
	SignatureBundle  string       `json:"signature_bundle,omitempty"`
	SignerIdentity   string       `json:"signer_identity,omitempty"`
	SignerOIDCIssuer string       `json:"signer_oidc_issuer,omitempty"`
	ExitCode         *int         `json:"exit_code"`
	Obligations      []string     `json:"obligations"`
}

type EvidenceBundle struct {
	Version      string                 `json:"version"`
	ManifestID   string                 `json:"manifest_id"`
	SpecsTouched []string               `json:"specs_touched"`
	ChangeRisk   Risk                   `json:"change_risk"`
	Artifact     EvidenceBundleArtifact `json:"artifact"`
	Runner       EvidenceBundleRunner   `json:"runner"`
	Timestamp    string                 `json:"timestamp"`
}

type EvidenceBundleArtifact struct {
	ID          string       `json:"id"`
	Kind        EvidenceKind `json:"kind"`
	Path        string       `json:"path"`
	SHA256      string       `json:"sha256"`
	Producer    string       `json:"producer,omitempty"`
	Command     string       `json:"command,omitempty"`
	ExitCode    int          `json:"exit_code"`
	Obligations []string     `json:"obligations"`
}

type EvidenceBundleRunner struct {
	Identity   string `json:"identity"`
	OIDCIssuer string `json:"oidc_issuer"`
	AgentName  string `json:"agent_name,omitempty"`
	AgentRunID string `json:"agent_run_id,omitempty"`
	AgentModel string `json:"agent_model,omitempty"`
}

type TestResults struct {
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

type ManifestRuntime struct {
	Metrics []string `json:"metrics"`
	Canary  Canary   `json:"canary"`
}

type Canary struct {
	Enabled        bool `json:"enabled"`
	InitialPercent int  `json:"initial_percent"`
}

type ManifestRollback struct {
	Strategy     string   `json:"strategy"`
	Flag         string   `json:"flag"`
	Compensation []string `json:"compensation"`
}

type Finding struct {
	Verifier    string   `json:"verifier"`
	Severity    string   `json:"severity"`
	Decision    string   `json:"decision"`
	Claim       string   `json:"claim"`
	Evidence    string   `json:"evidence"`
	RequiredFix string   `json:"required_fix"`
	Obligations []string `json:"obligations,omitempty"`
}

func (f Finding) Blocks() bool {
	return f.Decision == "block"
}

type VerifierOutput struct {
	Version       string    `json:"version"`
	Verifier      string    `json:"verifier"`
	PromptVersion string    `json:"prompt_version"`
	Model         string    `json:"model"`
	Obligations   []string  `json:"obligations"`
	Confidence    float64   `json:"confidence,omitempty"`
	Findings      []Finding `json:"findings"`
	Disagreements []string  `json:"disagreements,omitempty"`
}

type ReleasePolicy struct {
	Version string       `json:"version"`
	Rules   []PolicyRule `json:"rules"`
}

type PolicyRule struct {
	ID           string          `json:"id"`
	Description  string          `json:"description,omitempty"`
	When         PolicyCondition `json:"when"`
	Decision     string          `json:"decision"`
	Reasons      []string        `json:"reasons,omitempty"`
	ReasonSource string          `json:"reason_source,omitempty"`
	Stop         bool            `json:"stop,omitempty"`
}

type PolicyCondition struct {
	Fact        string            `json:"fact,omitempty"`
	RiskAtLeast Risk              `json:"risk_at_least,omitempty"`
	RiskBelow   Risk              `json:"risk_below,omitempty"`
	All         []PolicyCondition `json:"all,omitempty"`
	Any         []PolicyCondition `json:"any,omitempty"`
	Not         *PolicyCondition  `json:"not,omitempty"`
}

type PolicyInput struct {
	Version                  string              `json:"version"`
	Manifest                 Manifest            `json:"manifest"`
	Compilation              CompilationStats    `json:"compilation"`
	Risk                     Risk                `json:"risk"`
	RiskRank                 int                 `json:"risk_rank"`
	CanaryEnabled            bool                `json:"canary_enabled"`
	SignedEvidenceRequired   bool                `json:"signed_evidence_required"`
	SpecErrors               []string            `json:"spec_errors"`
	ManifestErrors           []string            `json:"manifest_errors"`
	ArtifactResults          []ArtifactResult    `json:"artifact_results"`
	InvalidEvidence          []InvalidEvidence   `json:"invalid_evidence"`
	VerifierOutputs          []VerifierOutput    `json:"verifier_outputs"`
	MissingObligations       map[string][]string `json:"missing_obligations"`
	CoveredObligations       map[string][]string `json:"covered_obligations"`
	Findings                 []Finding           `json:"findings"`
	BlockingFindings         []Finding           `json:"blocking_findings"`
	BlockingFindingClaims    []string            `json:"blocking_finding_claims"`
	HasInvalidSpecOrManifest bool                `json:"has_invalid_spec_or_manifest"`
	HasInvalidEvidence       bool                `json:"has_invalid_evidence"`
	HasMissingObligations    bool                `json:"has_missing_obligations"`
	HasBlockingFindings      bool                `json:"has_blocking_findings"`
}

type PolicyResult struct {
	Version         string   `json:"version"`
	PolicyPath      string   `json:"policy_path"`
	Decision        string   `json:"decision"`
	Reasons         []string `json:"reasons"`
	RulesFired      []string `json:"rules_fired"`
	FiredPolicyRule string   `json:"fired_policy_rule"`
}

type PolicySimulation struct {
	Version    string       `json:"version"`
	PolicyPath string       `json:"policy_path"`
	Input      PolicyInput  `json:"input"`
	Result     PolicyResult `json:"result"`
}

type Evidence struct {
	Version                string                      `json:"version"`
	Repo                   string                      `json:"repo"`
	ManifestPath           string                      `json:"manifest_path"`
	Compilation            CompilationStats            `json:"compilation"`
	Manifest               Manifest                    `json:"manifest"`
	Specs                  map[string]Spec             `json:"specs"`
	IRs                    map[string]IR               `json:"irs"`
	VerificationPlans      map[string]VerificationPlan `json:"verification_plans"`
	Diagnostics            []Diagnostic                `json:"diagnostics"`
	SignedEvidenceRequired bool                        `json:"signed_evidence_required"`
	PolicyPath             string                      `json:"policy_path"`
	PolicyResult           PolicyResult                `json:"policy_result"`
	SpecErrors             []string                    `json:"spec_errors"`
	ManifestErrors         []string                    `json:"manifest_errors"`
	ArtifactResults        []ArtifactResult            `json:"artifact_results"`
	InvalidEvidence        []InvalidEvidence           `json:"invalid_evidence"`
	VerifierOutputs        []VerifierOutput            `json:"verifier_outputs"`
	RequiredObligations    map[string][]Obligation     `json:"required_obligations"`
	CoveredObligations     map[string][]Obligation     `json:"covered_obligations"`
	MissingObligations     map[string][]Obligation     `json:"missing_obligations"`
	RequiredTests          map[string][]string         `json:"required_tests"`
	CoveredTests           map[string][]string         `json:"covered_tests"`
	MissingTests           map[string][]string         `json:"missing_tests"`
	RequiredSecurity       map[string][]string         `json:"required_security"`
	CoveredSecurity        map[string][]string         `json:"covered_security"`
	MissingSecurity        map[string][]string         `json:"missing_security"`
	Findings               []Finding                   `json:"findings"`
	Decision               string                      `json:"decision"`
	Reasons                []string                    `json:"reasons"`
}

type EvidenceManifest struct {
	Version      string                 `json:"version"`
	ArtifactType string                 `json:"artifact_type"`
	ArtifactPath string                 `json:"artifact_path"`
	Links        []EvidenceManifestLink `json:"links"`
}

type EvidenceManifestLink struct {
	ObligationID     string       `json:"obligation_id"`
	ArtifactType     string       `json:"artifact_type"`
	ArtifactPath     string       `json:"artifact_path"`
	Testcase         string       `json:"testcase"`
	Status           string       `json:"status"`
	Component        string       `json:"component"`
	RequiredEvidence EvidenceKind `json:"required_evidence"`
}

type EvidenceImportResult struct {
	Version              string                 `json:"version"`
	InputPath            string                 `json:"input_path"`
	OutputPath           string                 `json:"output_path"`
	ArtifactType         string                 `json:"artifact_type"`
	Links                []EvidenceManifestLink `json:"links"`
	CoveredObligations   []string               `json:"covered_obligations"`
	UnmatchedObligations []string               `json:"unmatched_obligations"`
}

type CompilationStats struct {
	SpecsLoaded      int `json:"specs_loaded"`
	SpecsCompiled    int `json:"specs_compiled"`
	SpecsSkipped     int `json:"specs_skipped"`
	ObligationsBuilt int `json:"obligations_built"`
}

type ArtifactResult struct {
	ID                 string          `json:"id"`
	Kind               EvidenceKind    `json:"kind"`
	Path               string          `json:"path,omitempty"`
	ResolvedPath       string          `json:"resolved_path,omitempty"`
	Status             string          `json:"status"`
	HashVerified       bool            `json:"hash_verified"`
	BundleVerified     bool            `json:"bundle_verified"`
	SignatureVerified  bool            `json:"signature_verified"`
	CoveredObligations []string        `json:"covered_obligations"`
	VerifierOutput     *VerifierOutput `json:"-"`
	VerifierFindings   []Finding       `json:"verifier_findings,omitempty"`
	FailedTests        []string        `json:"failed_tests,omitempty"`
	Issues             []string        `json:"issues,omitempty"`
}

type InvalidEvidence struct {
	Artifact string `json:"artifact"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

type GateResult struct {
	Version            string              `json:"version"`
	Decision           string              `json:"decision"`
	Reasons            []string            `json:"reasons"`
	PolicyPath         string              `json:"policy_path"`
	Compilation        CompilationStats    `json:"compilation"`
	SpecErrors         []string            `json:"spec_errors"`
	ManifestErrors     []string            `json:"manifest_errors"`
	ArtifactResults    []ArtifactResult    `json:"artifact_results"`
	InvalidEvidence    []InvalidEvidence   `json:"invalid_evidence"`
	MissingObligations map[string][]string `json:"missing_obligations"`
	CoveredObligations map[string][]string `json:"covered_obligations"`
	PolicyRulesFired   []string            `json:"policy_rules_fired"`
	FiredPolicyRule    string              `json:"fired_policy_rule"`
}

type Config struct {
	Version        string          `json:"version"`
	Profiles       []string        `json:"profiles"`
	Commands       []string        `json:"commands"`
	ArtifactDir    string          `json:"artifact_dir"`
	ManifestDir    string          `json:"manifest_dir"`
	BuildDir       string          `json:"build_dir"`
	IgnoredPaths   []string        `json:"ignored_paths"`
	AllowedSigners []AllowedSigner `json:"allowed_signers"`
}

type InitResult struct {
	Version     string   `json:"version"`
	Repo        string   `json:"repo"`
	ConfigPath  string   `json:"config_path"`
	Profiles    []string `json:"profiles"`
	Commands    []string `json:"commands"`
	CreatedDirs []string `json:"created_dirs"`
	Created     bool     `json:"created"`
}

type AllowedSigner struct {
	Identity   string `json:"identity"`
	OIDCIssuer string `json:"oidc_issuer"`
}

type ContractSuggestion struct {
	Name       string   `json:"name"`
	Profile    string   `json:"profile"`
	OwnedPaths []string `json:"owned_paths"`
	TestPaths  []string `json:"test_paths"`
	Confidence string   `json:"confidence"`
}

type TestMap struct {
	Version  string              `json:"version"`
	Mappings map[string][]string `json:"mappings"`
}

type JUnitMapResult struct {
	Version            string   `json:"version"`
	InputPath          string   `json:"input_path"`
	OutputPath         string   `json:"output_path"`
	TestMapPath        string   `json:"test_map_path"`
	Cases              int      `json:"cases"`
	CoveredObligations []string `json:"covered_obligations"`
}

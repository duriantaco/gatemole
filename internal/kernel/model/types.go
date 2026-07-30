package model

import (
	"encoding/json"
	"time"
)

const (
	AgentImageVersion        = "gatemole.agent_image.v0"
	ExecutionContractVersion = "gatemole.execution_contract.v0"
	AgentRunVersion          = "gatemole.agent_run.v0"
	CapabilityGrantVersion   = "gatemole.capability_grant.v0"
	ActionRequestVersion     = "gatemole.action_request.v0"
	RunEventVersion          = "gatemole.run_event.v0"
	CheckpointVersion        = "gatemole.checkpoint.v0"
	PolicyDecisionVersion    = "gatemole.policy_decision.v0"
)

type PrincipalKind string

const (
	PrincipalHuman    PrincipalKind = "human"
	PrincipalService  PrincipalKind = "service"
	PrincipalAgent    PrincipalKind = "agent"
	PrincipalRun      PrincipalKind = "run"
	PrincipalOperator PrincipalKind = "operator"
)

type RunState string

const (
	RunCreated            RunState = "created"
	RunAdmitted           RunState = "admitted"
	RunRunning            RunState = "running"
	RunWaitingForEvent    RunState = "waiting_for_event"
	RunWaitingForAgent    RunState = "waiting_for_agent"
	RunWaitingForApproval RunState = "waiting_for_approval"
	RunBlocked            RunState = "blocked"
	RunFailed             RunState = "failed"
	RunCancelled          RunState = "cancelled"
	RunCompleted          RunState = "completed"
)

func (s RunState) Terminal() bool {
	switch s {
	case RunFailed, RunCancelled, RunCompleted:
		return true
	default:
		return false
	}
}

type PolicyDecisionKind string

const (
	DecisionAllow           PolicyDecisionKind = "allow"
	DecisionDeny            PolicyDecisionKind = "deny"
	DecisionRequireApproval PolicyDecisionKind = "require_approval"
	DecisionConstrain       PolicyDecisionKind = "constrain"
	DecisionPause           PolicyDecisionKind = "pause"
	DecisionTerminate       PolicyDecisionKind = "terminate"
)

type PolicySubjectType string

const (
	SubjectRun        PolicySubjectType = "run"
	SubjectAction     PolicySubjectType = "action"
	SubjectDelegation PolicySubjectType = "delegation"
	SubjectCheckpoint PolicySubjectType = "checkpoint"
	SubjectRelease    PolicySubjectType = "release"
)

type Principal struct {
	ID           string        `json:"id"`
	Kind         PrincipalKind `json:"kind"`
	Issuer       string        `json:"issuer,omitempty"`
	ClaimsDigest string        `json:"claims_digest,omitempty"`
}

type ArtifactRef struct {
	URI       string `json:"uri"`
	Digest    string `json:"digest"`
	MediaType string `json:"media_type,omitempty"`
}

type ResourceSelector struct {
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
}

type BudgetLimits struct {
	MaxInputTokens     *int64 `json:"max_input_tokens,omitempty"`
	MaxOutputTokens    *int64 `json:"max_output_tokens,omitempty"`
	MaxModelCalls      *int64 `json:"max_model_calls,omitempty"`
	MaxToolCalls       *int64 `json:"max_tool_calls,omitempty"`
	MaxCostMicros      *int64 `json:"max_cost_micros,omitempty"`
	MaxWallTimeSeconds *int64 `json:"max_wall_time_seconds,omitempty"`
}

type BudgetUsage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ModelCalls      int64 `json:"model_calls"`
	ToolCalls       int64 `json:"tool_calls"`
	CostMicros      int64 `json:"cost_micros"`
	WallTimeSeconds int64 `json:"wall_time_seconds"`
}

type RuntimeBinding struct {
	Adapter        string     `json:"adapter"`
	AdapterVersion string     `json:"adapter_version"`
	LeaseID        string     `json:"lease_id,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
}

type AgentRuntime struct {
	Adapter        string   `json:"adapter"`
	AdapterVersion string   `json:"adapter_version,omitempty"`
	Entrypoint     []string `json:"entrypoint"`
}

type ModelRequirement struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

type ToolDeclaration struct {
	Name       string   `json:"name"`
	Driver     string   `json:"driver"`
	Operations []string `json:"operations"`
}

type AgentImage struct {
	Version      string             `json:"version"`
	ID           string             `json:"id"`
	Digest       string             `json:"digest"`
	Runtime      AgentRuntime       `json:"runtime"`
	Models       []ModelRequirement `json:"models,omitempty"`
	Instructions []ArtifactRef      `json:"instructions,omitempty"`
	Tools        []ToolDeclaration  `json:"tools,omitempty"`
	ContractRefs []string           `json:"contract_refs,omitempty"`
	SourceDigest string             `json:"source_digest"`
	Publisher    Principal          `json:"publisher"`
	Signature    *ArtifactRef       `json:"signature,omitempty"`
}

type CapabilityConditions struct {
	ApprovalRequired    bool     `json:"approval_required,omitempty"`
	WorkspaceRoot       string   `json:"workspace_root,omitempty"`
	EnvironmentAllow    []string `json:"environment_allowlist,omitempty"`
	DataClassifications []string `json:"data_classifications,omitempty"`
	MaxOutputBytes      *int64   `json:"max_output_bytes,omitempty"`
}

type ContractResource struct {
	ID         string               `json:"id"`
	Selector   ResourceSelector     `json:"selector"`
	Operations []string             `json:"operations"`
	Conditions CapabilityConditions `json:"conditions,omitempty"`
}

type CheckpointRequirement struct {
	ID               string   `json:"id"`
	Trigger          string   `json:"trigger"`
	ApprovalRequired bool     `json:"approval_required,omitempty"`
	RequiredEvidence []string `json:"required_evidence,omitempty"`
}

type ContractObligation struct {
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	RequiredEvidence string `json:"required_evidence"`
}

type EscalationPolicy struct {
	Owner          Principal `json:"owner"`
	TimeoutSeconds *int64    `json:"timeout_seconds,omitempty"`
	OnTimeout      string    `json:"on_timeout"`
}

type ExecutionContract struct {
	Version            string                  `json:"version"`
	ID                 string                  `json:"id"`
	Digest             string                  `json:"digest"`
	Owner              Principal               `json:"owner"`
	Goal               string                  `json:"goal"`
	NonGoals           []string                `json:"non_goals,omitempty"`
	Risk               string                  `json:"risk"`
	Resources          []ContractResource      `json:"resources"`
	Budgets            BudgetLimits            `json:"budgets"`
	Deadline           *time.Time              `json:"deadline,omitempty"`
	MaxChildDepth      *int64                  `json:"max_child_depth,omitempty"`
	MaxChildren        *int64                  `json:"max_children,omitempty"`
	Checkpoints        []CheckpointRequirement `json:"checkpoints,omitempty"`
	Obligations        []ContractObligation    `json:"obligations,omitempty"`
	Escalation         *EscalationPolicy       `json:"escalation,omitempty"`
	ReleaseContractRef string                  `json:"release_contract_ref,omitempty"`
}

type AgentRun struct {
	Version                string          `json:"version"`
	ID                     string          `json:"id"`
	Namespace              string          `json:"namespace"`
	ImageDigest            string          `json:"image_digest"`
	ContractDigest         string          `json:"contract_digest"`
	Principal              Principal       `json:"principal"`
	DelegationChain        []Principal     `json:"delegation_chain"`
	ParentRunID            string          `json:"parent_run_id,omitempty"`
	State                  RunState        `json:"state"`
	StateReason            string          `json:"state_reason,omitempty"`
	Priority               int64           `json:"priority,omitempty"`
	Deadline               *time.Time      `json:"deadline,omitempty"`
	BudgetLimits           BudgetLimits    `json:"budget_limits"`
	BudgetUsage            BudgetUsage     `json:"budget_usage"`
	Workspace              string          `json:"workspace,omitempty"`
	Runtime                *RuntimeBinding `json:"runtime,omitempty"`
	CapabilityIDs          []string        `json:"capability_ids"`
	CheckpointID           string          `json:"checkpoint_id,omitempty"`
	OutstandingApprovalIDs []string        `json:"outstanding_approval_ids"`
	EventSequence          int64           `json:"event_sequence"`
	CreatedAt              time.Time       `json:"created_at"`
	UpdatedAt              time.Time       `json:"updated_at"`
	CompletedAt            *time.Time      `json:"completed_at,omitempty"`
}

type CapabilityGrant struct {
	Version            string               `json:"version"`
	ID                 string               `json:"id"`
	SubjectRunID       string               `json:"subject_run_id"`
	Issuer             Principal            `json:"issuer"`
	Resource           ResourceSelector     `json:"resource"`
	Operations         []string             `json:"operations"`
	Conditions         CapabilityConditions `json:"conditions,omitempty"`
	Delegable          bool                 `json:"delegable"`
	DelegationParentID string               `json:"delegation_parent_id,omitempty"`
	IssuedAt           time.Time            `json:"issued_at"`
	ExpiresAt          time.Time            `json:"expires_at"`
	RevokedAt          *time.Time           `json:"revoked_at,omitempty"`
	MaxUses            *int64               `json:"max_uses,omitempty"`
	Uses               int64                `json:"uses"`
	PolicyDecisionID   string               `json:"policy_decision_id"`
}

type ActionRequest struct {
	Version         string           `json:"version"`
	ID              string           `json:"id"`
	RunID           string           `json:"run_id"`
	Operation       string           `json:"operation"`
	Resource        ResourceSelector `json:"resource"`
	Arguments       json.RawMessage  `json:"arguments"`
	ArgumentsDigest string           `json:"arguments_digest"`
	IdempotencyKey  string           `json:"idempotency_key"`
	Intent          string           `json:"intent"`
	CapabilityHint  string           `json:"capability_hint,omitempty"`
	RequestedAt     time.Time        `json:"requested_at"`
	ParentActionID  string           `json:"parent_action_id,omitempty"`
	Attempt         int64            `json:"attempt,omitempty"`
}

type RunEvent struct {
	Version          string          `json:"version"`
	ID               string          `json:"id"`
	RunID            string          `json:"run_id"`
	Sequence         int64           `json:"sequence"`
	Type             string          `json:"type"`
	Actor            Principal       `json:"actor"`
	OccurredAt       time.Time       `json:"occurred_at"`
	Payload          json.RawMessage `json:"payload"`
	Artifacts        []ArtifactRef   `json:"artifacts,omitempty"`
	PolicyDecisionID string          `json:"policy_decision_id,omitempty"`
	PreviousDigest   string          `json:"previous_digest,omitempty"`
	Digest           string          `json:"digest"`
}

type Checkpoint struct {
	Version                   string         `json:"version"`
	ID                        string         `json:"id"`
	RunID                     string         `json:"run_id"`
	EventSequence             int64          `json:"event_sequence"`
	ImageDigest               string         `json:"image_digest"`
	ContractDigest            string         `json:"contract_digest"`
	Runtime                   RuntimeBinding `json:"runtime"`
	CreatedAt                 time.Time      `json:"created_at"`
	StateRef                  ArtifactRef    `json:"state_ref"`
	StateDigest               string         `json:"state_digest"`
	MemoryRefs                []ArtifactRef  `json:"memory_refs"`
	WorkspaceRef              *ArtifactRef   `json:"workspace_ref,omitempty"`
	PendingActionIDs          []string       `json:"pending_action_ids"`
	CompatibleAdapterVersions []string       `json:"compatible_adapter_versions"`
}

type PolicyDecision struct {
	Version      string             `json:"version"`
	ID           string             `json:"id"`
	RunID        string             `json:"run_id"`
	SubjectType  PolicySubjectType  `json:"subject_type"`
	SubjectID    string             `json:"subject_id"`
	Decision     PolicyDecisionKind `json:"decision"`
	PolicyDigest string             `json:"policy_digest"`
	FactsDigest  string             `json:"facts_digest"`
	RulesFired   []string           `json:"rules_fired"`
	Reasons      []string           `json:"reasons"`
	Constraints  map[string]any     `json:"constraints,omitempty"`
	EvaluatedAt  time.Time          `json:"evaluated_at"`
}

type RunCreatedPayload struct {
	Run AgentRun `json:"run"`
}

type RunStateChangedPayload struct {
	From   RunState `json:"from"`
	To     RunState `json:"to"`
	Reason string   `json:"reason,omitempty"`
}

type CapabilitiesGrantedPayload struct {
	ContractDigest string            `json:"contract_digest"`
	Grants         []CapabilityGrant `json:"grants"`
}

type ActionRequestedPayload struct {
	Request ActionRequest `json:"request"`
}

type ActionDecisionPayload struct {
	ActionID     string             `json:"action_id"`
	Decision     PolicyDecisionKind `json:"decision"`
	CapabilityID string             `json:"capability_id,omitempty"`
	Reason       string             `json:"reason"`
	ErrorCode    ErrorCode          `json:"error_code,omitempty"`
}

type ActionExecutingPayload struct {
	ActionID     string `json:"action_id"`
	CapabilityID string `json:"capability_id"`
}

type ActionResultPayload struct {
	ActionID     string    `json:"action_id"`
	CapabilityID string    `json:"capability_id"`
	Status       string    `json:"status"`
	ResultDigest string    `json:"result_digest,omitempty"`
	OutputBytes  int64     `json:"output_bytes,omitempty"`
	ErrorCode    ErrorCode `json:"error_code,omitempty"`
	Reason       string    `json:"reason,omitempty"`
}

package model

import (
	"encoding/json"
	"time"
)

const (
	AgentTransactionVersion   = "gatemole.agent_transaction.v0"
	EffectVersion             = "gatemole.effect.v0"
	VerificationResultVersion = "gatemole.verification_result.v0"
	ApprovalPackageVersion    = "gatemole.approval_package.v0"
	ApprovalDecisionVersion   = "gatemole.approval_decision.v0"
	CommitPlanVersion         = "gatemole.commit_plan.v0"
	TransactionEventVersion   = "gatemole.transaction_event.v0"
	AgentExecutionVersion     = "gatemole.agent_execution.v0"
)

type TransactionState string

const (
	TransactionCreated                TransactionState = "created"
	TransactionRunning                TransactionState = "running"
	TransactionCompletedNoEffect      TransactionState = "completed_no_effect"
	TransactionStaged                 TransactionState = "staged"
	TransactionValidating             TransactionState = "validating"
	TransactionValidationFailed       TransactionState = "validation_failed"
	TransactionReviseRequired         TransactionState = "revise_required"
	TransactionBlocked                TransactionState = "blocked"
	TransactionPendingApproval        TransactionState = "pending_approval"
	TransactionReadyToCommit          TransactionState = "ready_to_commit"
	TransactionCommitting             TransactionState = "committing"
	TransactionCommitted              TransactionState = "committed"
	TransactionCompensating           TransactionState = "compensating"
	TransactionRolledBack             TransactionState = "rolled_back"
	TransactionReleaseFailed          TransactionState = "release_failed"
	TransactionPartiallyCommitted     TransactionState = "partially_committed"
	TransactionManualRecoveryRequired TransactionState = "manual_recovery_required"
	TransactionAborted                TransactionState = "aborted"
)

func (s TransactionState) Terminal() bool {
	switch s {
	case TransactionCompletedNoEffect, TransactionBlocked, TransactionCommitted, TransactionRolledBack,
		TransactionReleaseFailed,
		TransactionPartiallyCommitted, TransactionManualRecoveryRequired,
		TransactionAborted:
		return true
	default:
		return false
	}
}

type EffectRecoveryClass string

const (
	RecoveryReadOnly      EffectRecoveryClass = "read_only"
	RecoveryStageable     EffectRecoveryClass = "stageable"
	RecoveryReversible    EffectRecoveryClass = "reversible"
	RecoveryCompensatable EffectRecoveryClass = "compensatable"
	RecoveryIrreversible  EffectRecoveryClass = "irreversible"
)

type EffectStatus string

const (
	EffectProposed               EffectStatus = "proposed"
	EffectStaged                 EffectStatus = "staged"
	EffectValidated              EffectStatus = "validated"
	EffectReleaseReady           EffectStatus = "release_ready"
	EffectCommitting             EffectStatus = "committing"
	EffectCommitted              EffectStatus = "committed"
	EffectBlocked                EffectStatus = "blocked"
	EffectRejected               EffectStatus = "rejected"
	EffectFailed                 EffectStatus = "failed"
	EffectUnknown                EffectStatus = "unknown"
	EffectCompensating           EffectStatus = "compensating"
	EffectCompensated            EffectStatus = "compensated"
	EffectManualRecoveryRequired EffectStatus = "manual_recovery_required"
)

func (s EffectStatus) Terminal() bool {
	switch s {
	case EffectCommitted, EffectBlocked, EffectRejected, EffectFailed,
		EffectCompensated, EffectManualRecoveryRequired:
		return true
	default:
		return false
	}
}

type VerificationKind string

const (
	VerificationPrecondition  VerificationKind = "precondition"
	VerificationInvariant     VerificationKind = "invariant"
	VerificationPostcondition VerificationKind = "postcondition"
)

type VerificationStatus string

const (
	VerificationPassed        VerificationStatus = "passed"
	VerificationFailed        VerificationStatus = "failed"
	VerificationIndeterminate VerificationStatus = "indeterminate"
)

type VerificationIndependence string

const (
	VerificationAgentSupplied       VerificationIndependence = "agent_supplied"
	VerificationPlatformRun         VerificationIndependence = "platform_run"
	VerificationExternalAttestation VerificationIndependence = "external_attestation"
)

type AgentExecutionStatus string

const (
	AgentExecutionRunning     AgentExecutionStatus = "running"
	AgentExecutionSucceeded   AgentExecutionStatus = "succeeded"
	AgentExecutionFailed      AgentExecutionStatus = "failed"
	AgentExecutionInterrupted AgentExecutionStatus = "interrupted"
	AgentExecutionStartFailed AgentExecutionStatus = "start_failed"
)

func (s AgentExecutionStatus) Terminal() bool {
	switch s {
	case AgentExecutionSucceeded, AgentExecutionFailed,
		AgentExecutionInterrupted, AgentExecutionStartFailed:
		return true
	default:
		return false
	}
}

// AgentExecution is a privacy-preserving receipt for one supervised process.
// CommandDigest binds the complete argv without persisting arguments that may
// contain secrets. Program is retained for operator readability.
type AgentExecution struct {
	Version             string                `json:"version"`
	ID                  string                `json:"id"`
	TransactionID       string                `json:"transaction_id"`
	Attempt             int64                 `json:"attempt,omitempty"`
	RunID               string                `json:"run_id"`
	StageBindingID      string                `json:"stage_binding_id"`
	Program             string                `json:"program"`
	CommandDigest       string                `json:"command_digest"`
	RuntimeClass        string                `json:"runtime_class"`
	RuntimeConfigDigest string                `json:"runtime_config_digest"`
	ImageDigest         string                `json:"image_digest,omitempty"`
	TaskDigest          string                `json:"task_digest,omitempty"`
	ModelBroker         *ModelBrokerExecution `json:"model_broker,omitempty"`
	Status              AgentExecutionStatus  `json:"status"`
	ExitCode            *int                  `json:"exit_code,omitempty"`
	StdoutDigest        string                `json:"stdout_digest,omitempty"`
	StderrDigest        string                `json:"stderr_digest,omitempty"`
	StartedAt           time.Time             `json:"started_at"`
	CompletedAt         *time.Time            `json:"completed_at,omitempty"`
}

type ModelBrokerExecution struct {
	Provider            string `json:"provider"`
	ImageDigest         string `json:"image_digest"`
	PolicyDigest        string `json:"policy_digest"`
	ReceiptLedgerDigest string `json:"receipt_ledger_digest,omitempty"`
	Calls               int64  `json:"calls"`
	CompletedCalls      int64  `json:"completed_calls"`
	FailedCalls         int64  `json:"failed_calls"`
	UnknownCalls        int64  `json:"unknown_calls"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
}

// TransactionAdmissionBinding pins the task, run, and execution contract that
// were admitted together. It is optional so transaction projections created
// before atomic task admission remain readable.
type TransactionAdmissionBinding struct {
	RuntimeID          string `json:"runtime_id,omitempty"`
	EnforcementProfile string `json:"enforcement_profile,omitempty"`
	TaskDigest         string `json:"task_digest"`
	RunID              string `json:"run_id"`
	ContractDigest     string `json:"contract_digest"`
}

type AgentTransaction struct {
	Version                string                       `json:"version"`
	ID                     string                       `json:"id"`
	Namespace              string                       `json:"namespace"`
	Attempt                int64                        `json:"attempt,omitempty"`
	IntentDigest           string                       `json:"intent_digest"`
	Task                   *AgentTask                   `json:"task,omitempty"`
	Admission              *TransactionAdmissionBinding `json:"admission,omitempty"`
	Sponsor                Principal                    `json:"sponsor"`
	AgentRunIDs            []string                     `json:"agent_run_ids"`
	StageBindings          []StageBinding               `json:"stage_bindings"`
	State                  TransactionState             `json:"state"`
	StateReason            string                       `json:"state_reason,omitempty"`
	EffectIDs              []string                     `json:"effect_ids"`
	VerificationResultIDs  []string                     `json:"verification_result_ids"`
	OutstandingApprovalIDs []string                     `json:"outstanding_approval_ids"`
	StagedStateDigest      string                       `json:"staged_state_digest,omitempty"`
	EffectSetDigest        string                       `json:"effect_set_digest,omitempty"`
	ApprovalPackageDigest  string                       `json:"approval_package_digest,omitempty"`
	CommitPlanDigest       string                       `json:"commit_plan_digest,omitempty"`
	EventSequence          int64                        `json:"event_sequence"`
	CreatedAt              time.Time                    `json:"created_at"`
	UpdatedAt              time.Time                    `json:"updated_at"`
	CompletedAt            *time.Time                   `json:"completed_at,omitempty"`
}

// StageBinding records an isolated resource boundary owned by a transaction.
// Location is an operational locator, while BaseRevision binds its initial
// state. Connector-specific integrity remains enforced by its driver.
type StageBinding struct {
	ID           string           `json:"id"`
	Kind         string           `json:"kind"`
	Resource     ResourceSelector `json:"resource"`
	Location     string           `json:"location"`
	BaseRevision string           `json:"base_revision"`
	CreatedAt    time.Time        `json:"created_at"`
}

type EffectScope struct {
	Rows    *int64 `json:"rows,omitempty"`
	Bytes   *int64 `json:"bytes,omitempty"`
	Objects *int64 `json:"objects,omitempty"`
}

type CompensationSpec struct {
	Operation       string          `json:"operation"`
	Arguments       json.RawMessage `json:"arguments"`
	ArgumentsDigest string          `json:"arguments_digest"`
	PlanRef         *ArtifactRef    `json:"plan_ref,omitempty"`
}

type EffectReceipt struct {
	Driver          string    `json:"driver"`
	OperationID     string    `json:"operation_id"`
	ResultDigest    string    `json:"result_digest"`
	ResourceVersion string    `json:"resource_version,omitempty"`
	CommittedAt     time.Time `json:"committed_at"`
}

type Effect struct {
	Version             string              `json:"version"`
	ID                  string              `json:"id"`
	TransactionID       string              `json:"transaction_id"`
	Attempt             int64               `json:"attempt,omitempty"`
	Sequence            int64               `json:"sequence"`
	RunID               string              `json:"run_id,omitempty"`
	OriginExecutionID   string              `json:"origin_execution_id,omitempty"`
	OriginActionID      string              `json:"origin_action_id,omitempty"`
	System              string              `json:"system"`
	Resource            ResourceSelector    `json:"resource"`
	Operation           string              `json:"operation"`
	Arguments           json.RawMessage     `json:"arguments"`
	ArgumentsDigest     string              `json:"arguments_digest"`
	Dependencies        []string            `json:"dependencies"`
	EstimatedScope      EffectScope         `json:"estimated_scope"`
	DataClassification  string              `json:"data_classification,omitempty"`
	RecoveryClass       EffectRecoveryClass `json:"recovery_class"`
	Status              EffectStatus        `json:"status"`
	IdempotencyKey      string              `json:"idempotency_key,omitempty"`
	StageRef            *ArtifactRef        `json:"stage_ref,omitempty"`
	BeforeStateRef      *ArtifactRef        `json:"before_state_ref,omitempty"`
	Compensation        *CompensationSpec   `json:"compensation,omitempty"`
	Receipt             *EffectReceipt      `json:"receipt,omitempty"`
	CompensationReceipt *EffectReceipt      `json:"compensation_receipt,omitempty"`
	CreatedAt           time.Time           `json:"created_at"`
	UpdatedAt           time.Time           `json:"updated_at"`
}

type VerificationResult struct {
	Version           string                   `json:"version"`
	ID                string                   `json:"id"`
	TransactionID     string                   `json:"transaction_id"`
	Attempt           int64                    `json:"attempt,omitempty"`
	Name              string                   `json:"name"`
	Kind              VerificationKind         `json:"kind"`
	Status            VerificationStatus       `json:"status"`
	EffectSetDigest   string                   `json:"effect_set_digest"`
	StagedStateDigest string                   `json:"staged_state_digest"`
	Verifier          Principal                `json:"verifier"`
	Independence      VerificationIndependence `json:"independence"`
	VerifierDigest    string                   `json:"verifier_digest"`
	InputsDigest      string                   `json:"inputs_digest"`
	Evidence          []ArtifactRef            `json:"evidence"`
	Summary           string                   `json:"summary"`
	EvaluatedAt       time.Time                `json:"evaluated_at"`
	ExpiresAt         *time.Time               `json:"expires_at,omitempty"`
}

type RiskFinding struct {
	Code      string   `json:"code"`
	Severity  string   `json:"severity"`
	Summary   string   `json:"summary"`
	EffectIDs []string `json:"effect_ids"`
}

type ApprovalPackage struct {
	Version                 string        `json:"version"`
	ID                      string        `json:"id"`
	TransactionID           string        `json:"transaction_id"`
	Attempt                 int64         `json:"attempt,omitempty"`
	IntentDigest            string        `json:"intent_digest"`
	EffectSetDigest         string        `json:"effect_set_digest"`
	StagedStateDigest       string        `json:"staged_state_digest"`
	PolicyDigest            string        `json:"policy_digest"`
	CommitPlanDigest        string        `json:"commit_plan_digest"`
	VerificationResultIDs   []string      `json:"verification_result_ids"`
	RiskFindings            []RiskFinding `json:"risk_findings"`
	RequiredApprovalClasses []string      `json:"required_approval_classes"`
	Summary                 string        `json:"summary"`
	Digest                  string        `json:"digest"`
	CreatedAt               time.Time     `json:"created_at"`
	ExpiresAt               time.Time     `json:"expires_at"`
}

type ApprovalDecisionKind string

const (
	ApprovalApprove ApprovalDecisionKind = "approve"
	ApprovalReject  ApprovalDecisionKind = "reject"
	ApprovalRevise  ApprovalDecisionKind = "revise"
)

// ApprovalDecision is a signed, single-use authorization over one immutable
// approval package. Private key material never crosses the daemon boundary.
type ApprovalDecision struct {
	Version         string               `json:"version"`
	ID              string               `json:"id"`
	TransactionID   string               `json:"transaction_id"`
	ApprovalID      string               `json:"approval_id"`
	PackageDigest   string               `json:"package_digest"`
	ApprovalClass   string               `json:"approval_class"`
	Decision        ApprovalDecisionKind `json:"decision"`
	Reason          string               `json:"reason,omitempty"`
	Approver        Principal            `json:"approver"`
	KeyID           string               `json:"key_id"`
	IssuedAt        time.Time            `json:"issued_at"`
	ExpiresAt       time.Time            `json:"expires_at"`
	Nonce           string               `json:"nonce"`
	Digest          string               `json:"digest"`
	SignatureBase64 string               `json:"signature_base64"`
}

type CommitStep struct {
	Sequence        int64    `json:"sequence"`
	EffectID        string   `json:"effect_id"`
	DependsOn       []string `json:"depends_on"`
	PreconditionIDs []string `json:"precondition_ids"`
	IdempotencyKey  string   `json:"idempotency_key,omitempty"`
	TimeoutSeconds  int64    `json:"timeout_seconds"`
	RequiresReceipt bool     `json:"requires_receipt"`
}

type CompensationStep struct {
	Sequence        int64        `json:"sequence"`
	EffectID        string       `json:"effect_id"`
	PreconditionIDs []string     `json:"precondition_ids"`
	PlanRef         *ArtifactRef `json:"plan_ref,omitempty"`
}

type CommitPlan struct {
	Version                 string             `json:"version"`
	ID                      string             `json:"id"`
	TransactionID           string             `json:"transaction_id"`
	Attempt                 int64              `json:"attempt,omitempty"`
	IntentDigest            string             `json:"intent_digest"`
	EffectSetDigest         string             `json:"effect_set_digest"`
	StagedStateDigest       string             `json:"staged_state_digest"`
	PolicyDigest            string             `json:"policy_digest"`
	Connector               string             `json:"connector"`
	Target                  ResourceSelector   `json:"target"`
	ExpectedResourceVersion string             `json:"expected_resource_version"`
	Steps                   []CommitStep       `json:"steps"`
	CompensationSteps       []CompensationStep `json:"compensation_steps"`
	IrreversibleEffectIDs   []string           `json:"irreversible_effect_ids"`
	ManualRecoveryRef       *ArtifactRef       `json:"manual_recovery_ref,omitempty"`
	Digest                  string             `json:"digest"`
	CreatedAt               time.Time          `json:"created_at"`
}

// TransactionEvent is the authoritative append-only transaction history.
// Large or connector-specific values are content-addressed through artifacts.
type TransactionEvent struct {
	Version        string          `json:"version"`
	ID             string          `json:"id"`
	TransactionID  string          `json:"transaction_id"`
	Sequence       int64           `json:"sequence"`
	Type           string          `json:"type"`
	Actor          Principal       `json:"actor"`
	OccurredAt     time.Time       `json:"occurred_at"`
	Payload        json.RawMessage `json:"payload"`
	Artifacts      []ArtifactRef   `json:"artifacts,omitempty"`
	PreviousDigest string          `json:"previous_digest,omitempty"`
	Digest         string          `json:"digest"`
}

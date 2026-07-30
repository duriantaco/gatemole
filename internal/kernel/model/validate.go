package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	digestPattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	runtimeIDPattern  = regexp.MustCompile(`^runtime:[a-f0-9]{64}$`)
)

func IsSHA256Digest(value string) bool {
	return digestPattern.MatchString(value)
}

func IsIdentifier(value string) bool {
	return identifierPattern.MatchString(value)
}

func IsRuntimeID(value string) bool {
	return runtimeIDPattern.MatchString(value)
}

func IsEnforcementProfile(value string) bool {
	return value == "development" || value == "production"
}

func (image AgentImage) Validate() error {
	const resource = "AgentImage"
	if err := validateVersion(resource, image.Version, AgentImageVersion); err != nil {
		return err
	}
	if err := validateIdentifier(resource, "id", image.ID); err != nil {
		return err
	}
	if err := validateDigest(resource, "digest", image.Digest); err != nil {
		return err
	}
	if err := validateIdentifier(resource, "runtime.adapter", image.Runtime.Adapter); err != nil {
		return err
	}
	if len(image.Runtime.Entrypoint) == 0 {
		return invalid(resource, "runtime.entrypoint", "at least one entrypoint argument is required")
	}
	for i, value := range image.Runtime.Entrypoint {
		if strings.TrimSpace(value) == "" {
			return invalid(resource, fmt.Sprintf("runtime.entrypoint[%d]", i), "entrypoint arguments cannot be empty")
		}
	}
	for i, requirement := range image.Models {
		if err := validateIdentifier(resource, fmt.Sprintf("models[%d].provider", i), requirement.Provider); err != nil {
			return err
		}
		if strings.TrimSpace(requirement.Name) == "" {
			return invalid(resource, fmt.Sprintf("models[%d].name", i), "model name is required")
		}
	}
	for i, ref := range image.Instructions {
		if err := validateArtifactRef(resource, fmt.Sprintf("instructions[%d]", i), ref); err != nil {
			return err
		}
	}
	toolNames := map[string]struct{}{}
	for i, tool := range image.Tools {
		field := fmt.Sprintf("tools[%d]", i)
		if err := validateIdentifier(resource, field+".name", tool.Name); err != nil {
			return err
		}
		if _, exists := toolNames[tool.Name]; exists {
			return invalid(resource, field+".name", "tool names must be unique")
		}
		toolNames[tool.Name] = struct{}{}
		if err := validateIdentifier(resource, field+".driver", tool.Driver); err != nil {
			return err
		}
		if err := validateOperations(resource, field+".operations", tool.Operations); err != nil {
			return err
		}
	}
	if err := validateUniqueIdentifiers(resource, "contract_refs", image.ContractRefs); err != nil {
		return err
	}
	if err := validateDigest(resource, "source_digest", image.SourceDigest); err != nil {
		return err
	}
	if err := validatePrincipal(resource, "publisher", image.Publisher); err != nil {
		return err
	}
	if image.Signature != nil {
		if err := validateArtifactRef(resource, "signature", *image.Signature); err != nil {
			return err
		}
	}
	return nil
}

func (contract ExecutionContract) Validate() error {
	const resource = "ExecutionContract"
	if err := validateVersion(resource, contract.Version, ExecutionContractVersion); err != nil {
		return err
	}
	if err := validateIdentifier(resource, "id", contract.ID); err != nil {
		return err
	}
	if err := validateDigest(resource, "digest", contract.Digest); err != nil {
		return err
	}
	if err := validatePrincipal(resource, "owner", contract.Owner); err != nil {
		return err
	}
	if strings.TrimSpace(contract.Goal) == "" {
		return invalid(resource, "goal", "goal is required")
	}
	for i, value := range contract.NonGoals {
		if strings.TrimSpace(value) == "" {
			return invalid(resource, fmt.Sprintf("non_goals[%d]", i), "non-goals cannot be empty")
		}
	}
	if !validRisk(contract.Risk) {
		return invalid(resource, "risk", "risk must be low, medium, high, or critical")
	}
	resourceIDs := map[string]struct{}{}
	for i, declared := range contract.Resources {
		field := fmt.Sprintf("resources[%d]", i)
		if err := validateIdentifier(resource, field+".id", declared.ID); err != nil {
			return err
		}
		if _, exists := resourceIDs[declared.ID]; exists {
			return invalid(resource, field+".id", "resource IDs must be unique")
		}
		resourceIDs[declared.ID] = struct{}{}
		if err := validateResourceSelector(resource, field+".selector", declared.Selector); err != nil {
			return err
		}
		if err := validateOperations(resource, field+".operations", declared.Operations); err != nil {
			return err
		}
		if err := validateConditions(resource, field+".conditions", declared.Conditions); err != nil {
			return err
		}
	}
	if err := validateBudgetLimits(resource, "budgets", contract.Budgets); err != nil {
		return err
	}
	if contract.Deadline != nil && contract.Deadline.IsZero() {
		return invalid(resource, "deadline", "deadline cannot be the zero timestamp")
	}
	if err := validateOptionalNonnegative(resource, "max_child_depth", contract.MaxChildDepth); err != nil {
		return err
	}
	if err := validateOptionalNonnegative(resource, "max_children", contract.MaxChildren); err != nil {
		return err
	}
	checkpointIDs := map[string]struct{}{}
	for i, checkpoint := range contract.Checkpoints {
		field := fmt.Sprintf("checkpoints[%d]", i)
		if err := validateIdentifier(resource, field+".id", checkpoint.ID); err != nil {
			return err
		}
		if _, exists := checkpointIDs[checkpoint.ID]; exists {
			return invalid(resource, field+".id", "checkpoint IDs must be unique")
		}
		checkpointIDs[checkpoint.ID] = struct{}{}
		if strings.TrimSpace(checkpoint.Trigger) == "" {
			return invalid(resource, field+".trigger", "checkpoint trigger is required")
		}
		if err := validateUniqueIdentifiers(resource, field+".required_evidence", checkpoint.RequiredEvidence); err != nil {
			return err
		}
	}
	obligationIDs := map[string]struct{}{}
	for i, obligation := range contract.Obligations {
		field := fmt.Sprintf("obligations[%d]", i)
		if err := validateIdentifier(resource, field+".id", obligation.ID); err != nil {
			return err
		}
		if _, exists := obligationIDs[obligation.ID]; exists {
			return invalid(resource, field+".id", "obligation IDs must be unique")
		}
		obligationIDs[obligation.ID] = struct{}{}
		if err := validateIdentifier(resource, field+".kind", obligation.Kind); err != nil {
			return err
		}
		if err := validateIdentifier(resource, field+".required_evidence", obligation.RequiredEvidence); err != nil {
			return err
		}
	}
	if contract.Escalation != nil {
		if err := validatePrincipal(resource, "escalation.owner", contract.Escalation.Owner); err != nil {
			return err
		}
		if err := validateOptionalNonnegative(resource, "escalation.timeout_seconds", contract.Escalation.TimeoutSeconds); err != nil {
			return err
		}
		switch contract.Escalation.OnTimeout {
		case "block", "cancel", "escalate":
		default:
			return invalid(resource, "escalation.on_timeout", "on_timeout must be block, cancel, or escalate")
		}
	}
	if contract.ReleaseContractRef != "" {
		if err := validateIdentifier(resource, "release_contract_ref", contract.ReleaseContractRef); err != nil {
			return err
		}
	}
	return nil
}

func (run AgentRun) Validate() error {
	const resource = "AgentRun"
	if err := validateVersion(resource, run.Version, AgentRunVersion); err != nil {
		return err
	}
	if err := validateIdentifier(resource, "id", run.ID); err != nil {
		return err
	}
	if err := validateIdentifier(resource, "namespace", run.Namespace); err != nil {
		return err
	}
	if err := validateDigest(resource, "image_digest", run.ImageDigest); err != nil {
		return err
	}
	if err := validateDigest(resource, "contract_digest", run.ContractDigest); err != nil {
		return err
	}
	if err := validatePrincipal(resource, "principal", run.Principal); err != nil {
		return err
	}
	if run.Principal.Kind != PrincipalRun || run.Principal.ID != run.ID {
		return identityInvalid(resource, "principal", "run principal must have kind run and match the run ID")
	}
	for i, principal := range run.DelegationChain {
		if err := validatePrincipal(resource, fmt.Sprintf("delegation_chain[%d]", i), principal); err != nil {
			return err
		}
	}
	if run.ParentRunID != "" {
		if err := validateIdentifier(resource, "parent_run_id", run.ParentRunID); err != nil {
			return err
		}
	}
	if !validRunState(run.State) {
		return invalid(resource, "state", "unknown run state")
	}
	if stateRequiresReason(run.State) && strings.TrimSpace(run.StateReason) == "" {
		return invalid(resource, "state_reason", "blocked, failed, and cancelled states require a reason")
	}
	if run.Priority < 0 {
		return invalid(resource, "priority", "priority cannot be negative")
	}
	if run.Deadline != nil && run.Deadline.IsZero() {
		return invalid(resource, "deadline", "deadline cannot be the zero timestamp")
	}
	if err := validateBudgetLimits(resource, "budget_limits", run.BudgetLimits); err != nil {
		return err
	}
	if err := validateBudgetUsage(resource, "budget_usage", run.BudgetUsage, run.BudgetLimits); err != nil {
		return err
	}
	if run.Runtime != nil {
		if err := validateRuntimeBinding(resource, "runtime", *run.Runtime); err != nil {
			return err
		}
	}
	if err := validateUniqueIdentifiers(resource, "capability_ids", run.CapabilityIDs); err != nil {
		return err
	}
	if run.CheckpointID != "" {
		if err := validateIdentifier(resource, "checkpoint_id", run.CheckpointID); err != nil {
			return err
		}
	}
	if err := validateUniqueIdentifiers(resource, "outstanding_approval_ids", run.OutstandingApprovalIDs); err != nil {
		return err
	}
	if run.EventSequence < 1 {
		return invalid(resource, "event_sequence", "event sequence must start at 1")
	}
	if run.CreatedAt.IsZero() || run.UpdatedAt.IsZero() {
		return invalid(resource, "created_at", "created_at and updated_at are required")
	}
	if run.UpdatedAt.Before(run.CreatedAt) {
		return invalid(resource, "updated_at", "updated_at cannot precede created_at")
	}
	if run.State.Terminal() {
		if run.CompletedAt == nil || run.CompletedAt.IsZero() {
			return invalid(resource, "completed_at", "terminal states require completed_at")
		}
		if run.CompletedAt.Before(run.CreatedAt) {
			return invalid(resource, "completed_at", "completed_at cannot precede created_at")
		}
	} else if run.CompletedAt != nil {
		return invalid(resource, "completed_at", "non-terminal states cannot have completed_at")
	}
	return nil
}

func (grant CapabilityGrant) Validate() error {
	const resource = "CapabilityGrant"
	if err := validateVersion(resource, grant.Version, CapabilityGrantVersion); err != nil {
		return err
	}
	if err := validateIdentifier(resource, "id", grant.ID); err != nil {
		return err
	}
	if err := validateIdentifier(resource, "subject_run_id", grant.SubjectRunID); err != nil {
		return err
	}
	if err := validatePrincipal(resource, "issuer", grant.Issuer); err != nil {
		return err
	}
	if err := validateResourceSelector(resource, "resource", grant.Resource); err != nil {
		return err
	}
	if err := validateOperations(resource, "operations", grant.Operations); err != nil {
		return err
	}
	if err := validateConditions(resource, "conditions", grant.Conditions); err != nil {
		return err
	}
	if grant.DelegationParentID != "" {
		if err := validateIdentifier(resource, "delegation_parent_id", grant.DelegationParentID); err != nil {
			return err
		}
	}
	if grant.IssuedAt.IsZero() || grant.ExpiresAt.IsZero() {
		return invalid(resource, "issued_at", "issued_at and expires_at are required")
	}
	if !grant.ExpiresAt.After(grant.IssuedAt) {
		return invalid(resource, "expires_at", "expires_at must be after issued_at")
	}
	if grant.RevokedAt != nil && grant.RevokedAt.Before(grant.IssuedAt) {
		return invalid(resource, "revoked_at", "revoked_at cannot precede issued_at")
	}
	if grant.Uses < 0 {
		return invalid(resource, "uses", "uses cannot be negative")
	}
	if grant.MaxUses != nil {
		if *grant.MaxUses < 1 {
			return invalid(resource, "max_uses", "max_uses must be at least 1")
		}
		if grant.Uses > *grant.MaxUses {
			return invalid(resource, "uses", "uses cannot exceed max_uses")
		}
	}
	if err := validateIdentifier(resource, "policy_decision_id", grant.PolicyDecisionID); err != nil {
		return err
	}
	return nil
}

func (request ActionRequest) Validate() error {
	const resource = "ActionRequest"
	if err := validateVersion(resource, request.Version, ActionRequestVersion); err != nil {
		return err
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"id", request.ID},
		{"run_id", request.RunID},
		{"operation", request.Operation},
		{"idempotency_key", request.IdempotencyKey},
	} {
		if err := validateIdentifier(resource, item.field, item.value); err != nil {
			return err
		}
	}
	if err := validateResourceSelector(resource, "resource", request.Resource); err != nil {
		return err
	}
	if err := validateJSONObject(resource, "arguments", request.Arguments); err != nil {
		return err
	}
	if err := validateDigest(resource, "arguments_digest", request.ArgumentsDigest); err != nil {
		return err
	}
	if strings.TrimSpace(request.Intent) == "" {
		return invalid(resource, "intent", "intent is required")
	}
	if request.CapabilityHint != "" {
		if err := validateIdentifier(resource, "capability_hint", request.CapabilityHint); err != nil {
			return err
		}
	}
	if request.RequestedAt.IsZero() {
		return invalid(resource, "requested_at", "requested_at is required")
	}
	if request.ParentActionID != "" {
		if err := validateIdentifier(resource, "parent_action_id", request.ParentActionID); err != nil {
			return err
		}
	}
	if request.Attempt < 0 {
		return invalid(resource, "attempt", "attempt cannot be negative")
	}
	return nil
}

func (event RunEvent) Validate() error {
	const resource = "RunEvent"
	if err := validateVersion(resource, event.Version, RunEventVersion); err != nil {
		return err
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"id", event.ID},
		{"run_id", event.RunID},
		{"type", event.Type},
	} {
		if err := validateIdentifier(resource, item.field, item.value); err != nil {
			return err
		}
	}
	if event.Sequence < 1 {
		return newError(ErrorEventSequence, "validate", resource, "sequence", "event sequence must start at 1", nil)
	}
	if err := validatePrincipal(resource, "actor", event.Actor); err != nil {
		return err
	}
	if event.OccurredAt.IsZero() {
		return invalid(resource, "occurred_at", "occurred_at is required")
	}
	if err := validateJSONObject(resource, "payload", event.Payload); err != nil {
		return err
	}
	for i, ref := range event.Artifacts {
		if err := validateArtifactRef(resource, fmt.Sprintf("artifacts[%d]", i), ref); err != nil {
			return err
		}
	}
	if event.PolicyDecisionID != "" {
		if err := validateIdentifier(resource, "policy_decision_id", event.PolicyDecisionID); err != nil {
			return err
		}
	}
	if event.Sequence == 1 && event.PreviousDigest != "" {
		return newError(ErrorEventChain, "validate", resource, "previous_digest", "the first event cannot have a previous digest", nil)
	}
	if event.Sequence > 1 {
		if event.PreviousDigest == "" {
			return newError(ErrorEventChain, "validate", resource, "previous_digest", "events after sequence 1 require a previous digest", nil)
		}
		if err := validateDigest(resource, "previous_digest", event.PreviousDigest); err != nil {
			return err
		}
	}
	if err := validateDigest(resource, "digest", event.Digest); err != nil {
		return err
	}
	return nil
}

func (checkpoint Checkpoint) Validate() error {
	const resource = "Checkpoint"
	if err := validateVersion(resource, checkpoint.Version, CheckpointVersion); err != nil {
		return err
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"id", checkpoint.ID},
		{"run_id", checkpoint.RunID},
	} {
		if err := validateIdentifier(resource, item.field, item.value); err != nil {
			return err
		}
	}
	if checkpoint.EventSequence < 1 {
		return invalid(resource, "event_sequence", "event sequence must start at 1")
	}
	if err := validateDigest(resource, "image_digest", checkpoint.ImageDigest); err != nil {
		return err
	}
	if err := validateDigest(resource, "contract_digest", checkpoint.ContractDigest); err != nil {
		return err
	}
	if err := validateRuntimeBinding(resource, "runtime", checkpoint.Runtime); err != nil {
		return err
	}
	if checkpoint.CreatedAt.IsZero() {
		return invalid(resource, "created_at", "created_at is required")
	}
	if err := validateArtifactRef(resource, "state_ref", checkpoint.StateRef); err != nil {
		return err
	}
	if err := validateDigest(resource, "state_digest", checkpoint.StateDigest); err != nil {
		return err
	}
	for i, ref := range checkpoint.MemoryRefs {
		if err := validateArtifactRef(resource, fmt.Sprintf("memory_refs[%d]", i), ref); err != nil {
			return err
		}
	}
	if checkpoint.WorkspaceRef != nil {
		if err := validateArtifactRef(resource, "workspace_ref", *checkpoint.WorkspaceRef); err != nil {
			return err
		}
	}
	if err := validateUniqueIdentifiers(resource, "pending_action_ids", checkpoint.PendingActionIDs); err != nil {
		return err
	}
	if len(checkpoint.CompatibleAdapterVersions) == 0 {
		return invalid(resource, "compatible_adapter_versions", "at least one compatible adapter version is required")
	}
	if err := validateUniqueNonempty(resource, "compatible_adapter_versions", checkpoint.CompatibleAdapterVersions); err != nil {
		return err
	}
	return nil
}

func (decision PolicyDecision) Validate() error {
	const resource = "PolicyDecision"
	if err := validateVersion(resource, decision.Version, PolicyDecisionVersion); err != nil {
		return err
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"id", decision.ID},
		{"run_id", decision.RunID},
		{"subject_id", decision.SubjectID},
	} {
		if err := validateIdentifier(resource, item.field, item.value); err != nil {
			return err
		}
	}
	if !validSubjectType(decision.SubjectType) {
		return invalid(resource, "subject_type", "unknown policy subject type")
	}
	if !validDecision(decision.Decision) {
		return invalid(resource, "decision", "unknown policy decision")
	}
	if err := validateDigest(resource, "policy_digest", decision.PolicyDigest); err != nil {
		return err
	}
	if err := validateDigest(resource, "facts_digest", decision.FactsDigest); err != nil {
		return err
	}
	if len(decision.RulesFired) == 0 {
		return invalid(resource, "rules_fired", "at least one fired rule is required")
	}
	if err := validateUniqueIdentifiers(resource, "rules_fired", decision.RulesFired); err != nil {
		return err
	}
	if len(decision.Reasons) == 0 {
		return invalid(resource, "reasons", "at least one reason is required")
	}
	for i, reason := range decision.Reasons {
		if strings.TrimSpace(reason) == "" {
			return invalid(resource, fmt.Sprintf("reasons[%d]", i), "reasons cannot be empty")
		}
	}
	constraintKeys := make([]string, 0, len(decision.Constraints))
	for key := range decision.Constraints {
		constraintKeys = append(constraintKeys, key)
	}
	sort.Strings(constraintKeys)
	for _, key := range constraintKeys {
		value := decision.Constraints[key]
		if strings.TrimSpace(key) == "" {
			return invalid(resource, "constraints", "constraint names cannot be empty")
		}
		switch value.(type) {
		case nil, string, bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		default:
			return invalid(resource, "constraints."+key, "constraint values must be scalar")
		}
	}
	if decision.EvaluatedAt.IsZero() {
		return invalid(resource, "evaluated_at", "evaluated_at is required")
	}
	return nil
}

func validateVersion(resource, actual, expected string) error {
	if actual != expected {
		return invalid(resource, "version", fmt.Sprintf("expected %q", expected))
	}
	return nil
}

func validateIdentifier(resource, field, value string) error {
	if !identifierPattern.MatchString(value) {
		return invalid(resource, field, "must be a non-empty kernel identifier of at most 256 characters")
	}
	return nil
}

func validateDigest(resource, field, value string) error {
	if !digestPattern.MatchString(value) {
		return invalid(resource, field, "must be a lowercase sha256 digest")
	}
	return nil
}

func validatePrincipal(resource, field string, principal Principal) error {
	if err := validateIdentifier(resource, field+".id", principal.ID); err != nil {
		return identityInvalid(resource, field+".id", err.Error())
	}
	switch principal.Kind {
	case PrincipalHuman, PrincipalService, PrincipalAgent, PrincipalRun, PrincipalOperator:
	default:
		return identityInvalid(resource, field+".kind", "unknown principal kind")
	}
	if principal.ClaimsDigest != "" {
		if err := validateDigest(resource, field+".claims_digest", principal.ClaimsDigest); err != nil {
			return err
		}
	}
	return nil
}

func validateArtifactRef(resource, field string, ref ArtifactRef) error {
	if strings.TrimSpace(ref.URI) == "" {
		return invalid(resource, field+".uri", "artifact URI is required")
	}
	return validateDigest(resource, field+".digest", ref.Digest)
}

func validateResourceSelector(resource, field string, selector ResourceSelector) error {
	if err := validateIdentifier(resource, field+".kind", selector.Kind); err != nil {
		return err
	}
	if strings.TrimSpace(selector.Pattern) == "" {
		return invalid(resource, field+".pattern", "resource pattern is required")
	}
	return nil
}

func validateConditions(resource, field string, conditions CapabilityConditions) error {
	if err := validateUniqueNonempty(resource, field+".environment_allowlist", conditions.EnvironmentAllow); err != nil {
		return err
	}
	if err := validateUniqueNonempty(resource, field+".data_classifications", conditions.DataClassifications); err != nil {
		return err
	}
	return validateOptionalNonnegative(resource, field+".max_output_bytes", conditions.MaxOutputBytes)
}

func validateOperations(resource, field string, values []string) error {
	if len(values) == 0 {
		return invalid(resource, field, "at least one operation is required")
	}
	return validateUniqueIdentifiers(resource, field, values)
}

func validateUniqueIdentifiers(resource, field string, values []string) error {
	seen := map[string]struct{}{}
	for i, value := range values {
		if err := validateIdentifier(resource, fmt.Sprintf("%s[%d]", field, i), value); err != nil {
			return err
		}
		if _, exists := seen[value]; exists {
			return invalid(resource, field, "values must be unique")
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateUniqueNonempty(resource, field string, values []string) error {
	seen := map[string]struct{}{}
	for i, value := range values {
		if strings.TrimSpace(value) == "" {
			return invalid(resource, fmt.Sprintf("%s[%d]", field, i), "values cannot be empty")
		}
		if _, exists := seen[value]; exists {
			return invalid(resource, field, "values must be unique")
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateBudgetLimits(resource, field string, limits BudgetLimits) error {
	for _, item := range []struct {
		name  string
		value *int64
	}{
		{"max_input_tokens", limits.MaxInputTokens},
		{"max_output_tokens", limits.MaxOutputTokens},
		{"max_model_calls", limits.MaxModelCalls},
		{"max_tool_calls", limits.MaxToolCalls},
		{"max_cost_micros", limits.MaxCostMicros},
		{"max_wall_time_seconds", limits.MaxWallTimeSeconds},
	} {
		if err := validateOptionalNonnegative(resource, field+"."+item.name, item.value); err != nil {
			return err
		}
	}
	return nil
}

func validateBudgetUsage(resource, field string, usage BudgetUsage, limits BudgetLimits) error {
	values := []struct {
		name  string
		used  int64
		limit *int64
	}{
		{"input_tokens", usage.InputTokens, limits.MaxInputTokens},
		{"output_tokens", usage.OutputTokens, limits.MaxOutputTokens},
		{"model_calls", usage.ModelCalls, limits.MaxModelCalls},
		{"tool_calls", usage.ToolCalls, limits.MaxToolCalls},
		{"cost_micros", usage.CostMicros, limits.MaxCostMicros},
		{"wall_time_seconds", usage.WallTimeSeconds, limits.MaxWallTimeSeconds},
	}
	for _, value := range values {
		if value.used < 0 {
			return invalid(resource, field+"."+value.name, "budget usage cannot be negative")
		}
		if value.limit != nil && value.used > *value.limit {
			return newError(ErrorBudgetExceeded, "validate", resource, field+"."+value.name, "budget usage exceeds its hard limit", nil)
		}
	}
	return nil
}

func validateRuntimeBinding(resource, field string, runtime RuntimeBinding) error {
	if err := validateIdentifier(resource, field+".adapter", runtime.Adapter); err != nil {
		return err
	}
	if strings.TrimSpace(runtime.AdapterVersion) == "" {
		return invalid(resource, field+".adapter_version", "adapter version is required")
	}
	if runtime.LeaseID != "" {
		if err := validateIdentifier(resource, field+".lease_id", runtime.LeaseID); err != nil {
			return err
		}
		if runtime.LeaseExpiresAt == nil || runtime.LeaseExpiresAt.IsZero() {
			return invalid(resource, field+".lease_expires_at", "a lease ID requires an expiry")
		}
	} else if runtime.LeaseExpiresAt != nil {
		return invalid(resource, field+".lease_id", "a lease expiry requires a lease ID")
	}
	return nil
}

func validateJSONObject(resource, field string, data json.RawMessage) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return invalid(resource, field, "JSON object is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return newError(ErrorSchemaInvalid, "validate", resource, field, "must be a JSON object", err)
	}
	if value == nil {
		return invalid(resource, field, "must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return newError(ErrorSchemaInvalid, "validate", resource, field, "trailing JSON is not allowed", err)
	}
	return nil
}

func validateOptionalNonnegative(resource, field string, value *int64) error {
	if value != nil && *value < 0 {
		return invalid(resource, field, "value cannot be negative")
	}
	return nil
}

func validRisk(value string) bool {
	switch value {
	case "low", "medium", "high", "critical":
		return true
	default:
		return false
	}
}

func validRunState(state RunState) bool {
	switch state {
	case RunCreated, RunAdmitted, RunRunning, RunWaitingForEvent, RunWaitingForAgent,
		RunWaitingForApproval, RunBlocked, RunFailed, RunCancelled, RunCompleted:
		return true
	default:
		return false
	}
}

func stateRequiresReason(state RunState) bool {
	return state == RunBlocked || state == RunFailed || state == RunCancelled
}

func validDecision(value PolicyDecisionKind) bool {
	switch value {
	case DecisionAllow, DecisionDeny, DecisionRequireApproval, DecisionConstrain, DecisionPause, DecisionTerminate:
		return true
	default:
		return false
	}
}

func validSubjectType(value PolicySubjectType) bool {
	switch value {
	case SubjectRun, SubjectAction, SubjectDelegation, SubjectCheckpoint, SubjectRelease:
		return true
	default:
		return false
	}
}

func invalid(resource, field, message string) *KernelError {
	return newError(ErrorSchemaInvalid, "validate", resource, field, message, nil)
}

func identityInvalid(resource, field, message string) *KernelError {
	return newError(ErrorIdentityInvalid, "validate", resource, field, message, nil)
}

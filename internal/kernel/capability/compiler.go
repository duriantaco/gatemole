package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/reducer"
)

const defaultGrantTTL = time.Hour

// Compile lowers an execution contract's resource rules into immutable,
// time-bounded grants for one run. The contract digest must already be pinned
// by that run.
func Compile(contract model.ExecutionContract, run model.AgentRun, issuedAt time.Time) ([]model.CapabilityGrant, error) {
	if err := contract.Validate(); err != nil {
		return nil, err
	}
	if err := run.Validate(); err != nil {
		return nil, err
	}
	if contract.Digest != run.ContractDigest {
		return nil, kernelError(model.ErrorCapabilityDenied, "compile_capabilities", run.ID, "contract digest does not match the run")
	}
	if run.State.Terminal() {
		return nil, kernelError(model.ErrorCapabilityDenied, "compile_capabilities", run.ID, "terminal runs cannot receive capabilities")
	}
	issuedAt = issuedAt.UTC()
	if issuedAt.IsZero() {
		return nil, kernelError(model.ErrorSchemaInvalid, "compile_capabilities", run.ID, "issued_at is required")
	}
	expiresAt := issuedAt.Add(defaultGrantTTL)
	for _, deadline := range []*time.Time{run.Deadline, contract.Deadline} {
		if deadline != nil && deadline.Before(expiresAt) {
			expiresAt = deadline.UTC()
		}
	}
	if !expiresAt.After(issuedAt) {
		return nil, kernelError(model.ErrorCapabilityExpired, "compile_capabilities", run.ID, "run or contract deadline has expired")
	}

	decisionID := stableID("decision", contract.Digest, run.ID)
	grants := make([]model.CapabilityGrant, 0, len(contract.Resources))
	for _, resource := range contract.Resources {
		if resource.Selector.Kind == "filesystem" {
			if err := validateFilesystemScope(resource); err != nil {
				return nil, err
			}
		}
		grant := model.CapabilityGrant{
			Version:          model.CapabilityGrantVersion,
			ID:               stableID("cap", contract.Digest, run.ID, resource.ID),
			SubjectRunID:     run.ID,
			Issuer:           model.Principal{ID: "service:vouchd", Kind: model.PrincipalService, Issuer: "vouchd"},
			Resource:         resource.Selector,
			Operations:       append([]string(nil), resource.Operations...),
			Conditions:       resource.Conditions,
			Delegable:        false,
			IssuedAt:         issuedAt,
			ExpiresAt:        expiresAt,
			MaxUses:          cloneInt64(contract.Budgets.MaxToolCalls),
			Uses:             0,
			PolicyDecisionID: decisionID,
		}
		if err := grant.Validate(); err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	if len(grants) == 0 {
		return nil, kernelError(model.ErrorCapabilityDenied, "compile_capabilities", run.ID, "contract declares no resources")
	}
	return grants, nil
}

// GrantsFromEvents reconstructs installed grants and their authorized-use
// counts from the authoritative event history.
func GrantsFromEvents(events []model.RunEvent) (map[string]model.CapabilityGrant, map[string]int64, error) {
	grants := map[string]model.CapabilityGrant{}
	uses := map[string]int64{}
	for _, event := range events {
		switch event.Type {
		case reducer.EventCapabilitiesGranted:
			payload, err := model.DecodePayloadStrict[model.CapabilitiesGrantedPayload](event)
			if err != nil {
				return nil, nil, err
			}
			for _, grant := range payload.Grants {
				if err := grant.Validate(); err != nil {
					return nil, nil, err
				}
				if _, exists := grants[grant.ID]; exists {
					return nil, nil, kernelError(model.ErrorEventChain, "replay_capabilities", grant.ID, "duplicate capability grant")
				}
				grants[grant.ID] = grant
			}
		case reducer.EventActionAuthorized:
			payload, err := model.DecodePayloadStrict[model.ActionDecisionPayload](event)
			if err != nil {
				return nil, nil, err
			}
			uses[payload.CapabilityID]++
		}
	}
	return grants, uses, nil
}

func MatchResource(selector, target string) bool {
	cleanTarget, ok := CleanLogicalPath(target)
	if !ok {
		return false
	}
	if strings.HasSuffix(selector, "/**") {
		base := strings.TrimSuffix(selector, "/**")
		cleanBase, valid := CleanLogicalPath(base)
		return valid && (cleanTarget == cleanBase || strings.HasPrefix(cleanTarget, cleanBase+"/"))
	}
	cleanSelector := path.Clean(selector)
	if cleanSelector != selector || strings.Contains(selector, "\\") || path.IsAbs(selector) {
		return false
	}
	matched, err := path.Match(cleanSelector, cleanTarget)
	return err == nil && matched
}

func CleanLogicalPath(value string) (string, bool) {
	if value == "" || strings.Contains(value, "\\") || strings.ContainsRune(value, 0) || path.IsAbs(value) {
		return "", false
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return "", false
	}
	return clean, true
}

func WorkspaceRelative(workspaceRoot, target string) (string, bool) {
	root, valid := CleanLogicalPath(workspaceRoot)
	if !valid {
		return "", false
	}
	cleanTarget, valid := CleanLogicalPath(target)
	if !valid || cleanTarget == root {
		return "", false
	}
	prefix := root + "/"
	if !strings.HasPrefix(cleanTarget, prefix) {
		return "", false
	}
	return strings.TrimPrefix(cleanTarget, prefix), true
}

func validateFilesystemScope(resource model.ContractResource) error {
	root, valid := CleanLogicalPath(resource.Conditions.WorkspaceRoot)
	if !valid {
		return kernelError(model.ErrorSchemaInvalid, "compile_capabilities", resource.ID, "filesystem resources require a clean relative workspace_root")
	}
	pattern := resource.Selector.Pattern
	if pattern != root && !strings.HasPrefix(pattern, root+"/") {
		return kernelError(model.ErrorCapabilityDenied, "compile_capabilities", resource.ID, "filesystem selector escapes its workspace_root")
	}
	for _, operation := range resource.Operations {
		if operation != "filesystem.read" && operation != "filesystem.write" {
			return kernelError(model.ErrorSchemaInvalid, "compile_capabilities", resource.ID, fmt.Sprintf("unsupported filesystem operation %q", operation))
		}
	}
	return nil
}

func stableID(prefix string, values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return prefix + ":" + hex.EncodeToString(hash.Sum(nil)[:16])
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func kernelError(code model.ErrorCode, operation, resource, message string) *model.KernelError {
	return &model.KernelError{Code: code, Operation: operation, Resource: resource, Message: message}
}

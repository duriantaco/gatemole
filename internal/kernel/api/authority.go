package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/sandbox"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
	"github.com/duriantaco/vouch/internal/kernel/verification"
)

func (s *Server) currentAuthorityPolicyDigest() (string, error) {
	sequenceDigest, err := transactionreducer.PolicyDigest(s.sequencePolicy)
	if err != nil {
		return "", err
	}
	verifierProfileDigest := ""
	verifierRuntimeDigest := ""
	if s.executionPolicy.VerifierProfiles != nil {
		verifierProfileDigest =
			s.executionPolicy.VerifierProfilesSourceDigest
		if verifierProfileDigest == "" {
			verifierProfileDigest = s.executionPolicy.VerifierProfiles.Digest()
		}
		verifierRuntimeDigest, err =
			s.currentVerifierRuntimePolicyDigest()
		if err != nil {
			return "", err
		}
	}
	if verifierProfileDigest == "" &&
		s.executionPolicy.EnforcementProfile == "" &&
		s.executionPolicy.IdentityTrustDigest == "" &&
		s.executionPolicy.ApprovalTrustDigest == "" {
		return sequenceDigest, nil
	}
	data, err := json.Marshal(struct {
		Version               string `json:"version"`
		SequencePolicyDigest  string `json:"sequence_policy_digest"`
		VerifierProfileDigest string `json:"verifier_profile_digest,omitempty"`
		VerifierRuntimeDigest string `json:"verifier_runtime_digest,omitempty"`
		EnforcementProfile    string `json:"enforcement_profile,omitempty"`
		IdentityTrustDigest   string `json:"identity_trust_digest,omitempty"`
		ApprovalTrustDigest   string `json:"approval_trust_digest,omitempty"`
	}{
		Version:               "vouch.authority_policy.v0",
		SequencePolicyDigest:  sequenceDigest,
		VerifierProfileDigest: verifierProfileDigest,
		VerifierRuntimeDigest: verifierRuntimeDigest,
		EnforcementProfile:    s.executionPolicy.EnforcementProfile,
		IdentityTrustDigest:   s.executionPolicy.IdentityTrustDigest,
		ApprovalTrustDigest:   s.executionPolicy.ApprovalTrustDigest,
	})
	if err != nil {
		return "", fmt.Errorf("encode authority policy: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (s *Server) validateReleaseAuthority(
	ctx context.Context,
	namespace string,
	projection transactionreducer.Projection,
	releaser model.Principal,
	now time.Time,
) error {
	if projection.ApprovalPackage == nil || projection.CommitPlan == nil {
		return approvalAuthorityError(
			projection.Transaction.ID,
			"release authority is missing its immutable package or commit plan",
		)
	}
	pkg := *projection.ApprovalPackage
	if !pkg.ExpiresAt.After(now) {
		return approvalAuthorityError(
			projection.Transaction.ID,
			"approval package expired before release",
		)
	}
	currentPolicyDigest, err := s.currentAuthorityPolicyDigest()
	if err != nil {
		return &model.KernelError{
			Code:      model.ErrorInternal,
			Operation: "authorize_transaction",
			Resource:  projection.Transaction.ID,
			Message:   "current authority policy cannot be digested",
			Cause:     err,
		}
	}
	if pkg.PolicyDigest != currentPolicyDigest ||
		projection.CommitPlan.PolicyDigest != currentPolicyDigest {
		return approvalAuthorityError(
			projection.Transaction.ID,
			"authority policy changed after the transaction was prepared",
		)
	}
	if err := s.validateVerifierProfiles(projection); err != nil {
		return err
	}
	if len(pkg.RequiredApprovalClasses) == 0 {
		return nil
	}
	events, err := s.store.TransactionEvents(
		ctx, namespace, projection.Transaction.ID, 0,
	)
	if err != nil {
		return err
	}
	decisions, err := approvedDecisions(projection, events)
	if err != nil {
		return err
	}
	if err := validateReleaseSeparation(
		projection.Transaction.ID, releaser, decisions,
	); err != nil {
		return err
	}
	covered := make(map[string]struct{}, len(decisions))
	for _, decision := range decisions {
		if decision.Decision != model.ApprovalApprove {
			return approvalAuthorityError(
				projection.Transaction.ID,
				"non-approval decision cannot authorize release",
			)
		}
		if err := validateApprovalSeparation(
			projection, events, decision.Approver, decision.Digest,
		); err != nil {
			return err
		}
		if err := s.approvalTrust.Verify(decision, pkg, now); err != nil {
			return err
		}
		covered[decision.ApprovalClass] = struct{}{}
	}
	for _, class := range pkg.RequiredApprovalClasses {
		if _, ok := covered[class]; !ok {
			return approvalAuthorityError(
				projection.Transaction.ID,
				fmt.Sprintf("required approval class %q is not current", class),
			)
		}
	}
	return nil
}

func (s *Server) validateVerifierProfiles(
	projection transactionreducer.Projection,
) error {
	profiles := s.executionPolicy.VerifierProfiles
	if profiles == nil {
		if s.executionPolicy.RequireVerifierProfiles {
			return &model.KernelError{
				Code:      model.ErrorVerificationFailed,
				Operation: "authorize_transaction",
				Resource:  projection.Transaction.ID,
				Message:   "production authority requires daemon-owned verifier profiles",
			}
		}
		return nil
	}
	results := make(map[string]model.VerificationResult, len(projection.Verifications))
	for _, result := range projection.Verifications {
		results[result.Name] = result
	}
	for _, profile := range profiles.Profiles() {
		result, ok := results[profile.Name]
		expectedVerifierDigest, err := s.expectedVerifierDigest(
			profile,
		)
		if err != nil {
			return &model.KernelError{
				Code:      model.ErrorInternal,
				Operation: "authorize_transaction",
				Resource:  profile.Name,
				Message:   "current verifier runtime policy is invalid",
				Cause:     err,
			}
		}
		expectedInputsDigest, err :=
			transactionreducer.ComputeVerificationInputsDigest(
				projection.Transaction.EffectSetDigest,
				projection.Transaction.StagedStateDigest,
			)
		if err != nil {
			return &model.KernelError{
				Code:      model.ErrorInternal,
				Operation: "authorize_transaction",
				Resource:  profile.Name,
				Message:   "current verification inputs cannot be digested",
				Cause:     err,
			}
		}
		if !ok ||
			result.Status != model.VerificationPassed ||
			result.Independence != model.VerificationPlatformRun ||
			result.Verifier.ClaimsDigest != profile.Digest ||
			result.VerifierDigest != expectedVerifierDigest ||
			result.InputsDigest != expectedInputsDigest ||
			result.EffectSetDigest !=
				projection.Transaction.EffectSetDigest ||
			result.StagedStateDigest !=
				projection.Transaction.StagedStateDigest {
			return &model.KernelError{
				Code:      model.ErrorVerificationFailed,
				Operation: "authorize_transaction",
				Resource:  profile.Name,
				Message:   "required daemon-owned verifier profile is missing, stale, or not passing",
			}
		}
	}
	return nil
}

func (s *Server) currentVerifierRuntimePolicyDigest() (string, error) {
	profiles := s.executionPolicy.VerifierProfiles
	if profiles == nil {
		return "", nil
	}
	type runtimeEntry struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
	}
	entries := make([]runtimeEntry, 0, profiles.Len())
	for _, profile := range profiles.Profiles() {
		digest, err := s.expectedVerifierDigest(profile)
		if err != nil {
			return "", err
		}
		entries = append(entries, runtimeEntry{
			Name: profile.Name, Digest: digest,
		})
	}
	data, err := json.Marshal(struct {
		Version                string         `json:"version"`
		MaxConcurrentWorkloads int            `json:"max_concurrent_workloads"`
		Verifiers              []runtimeEntry `json:"verifiers"`
	}{
		Version: "vouch.verifier_runtime_policy.v0",
		MaxConcurrentWorkloads: s.executionPolicy.
			MaxConcurrentWorkloads,
		Verifiers: entries,
	})
	if err != nil {
		return "", fmt.Errorf("encode verifier runtime policy: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (s *Server) expectedVerifierDigest(
	profile verification.Profile,
) (string, error) {
	config := sandbox.OCIConfig{
		EnginePath:    s.executionPolicy.EnginePath,
		Image:         profile.Image,
		Workspace:     "/",
		TransactionID: "tx:verifier-policy",
		RunID:         "verifier:" + profile.Name,
		Command:       append([]string(nil), profile.Command...),
		UID:           s.executionPolicy.VerifierUID,
		GID:           s.executionPolicy.VerifierGID,
		MemoryBytes:   s.executionPolicy.VerifierMemoryBytes,
		CPUMillis:     s.executionPolicy.VerifierCPUMillis,
		PIDsLimit:     s.executionPolicy.VerifierPIDsLimit,
		TmpfsBytes:    s.executionPolicy.VerifierTmpfsBytes,
		ContainerName: "vouch-verifier-policy",
		Role:          "verifier",
		WorkspaceMode: "staged_ro",
	}
	runtimeDigest, err := config.RuntimeConfigDigest()
	if err != nil {
		return "", err
	}
	commandDigest, err := transactionreducer.ComputeCommandDigest(
		profile.Command,
	)
	if err != nil {
		return "", err
	}
	return transactionreducer.ComputeVerifierDigest(
		commandDigest,
		runtimeDigest,
		profile.ImageDigest,
	)
}

func validateApprovalSeparation(
	projection transactionreducer.Projection,
	events []model.TransactionEvent,
	approver model.Principal,
	approvalDigest string,
) error {
	if sameAuthority(approver, projection.Transaction.Sponsor) {
		return approvalAuthorityError(
			projection.Transaction.ID,
			"the transaction sponsor cannot approve the same transaction",
		)
	}
	for _, runID := range projection.Transaction.AgentRunIDs {
		if runID == approver.ID {
			return approvalAuthorityError(
				projection.Transaction.ID,
				"a participating agent cannot approve its own transaction",
			)
		}
	}
	for _, event := range events {
		if event.Type == transactionreducer.EventApprovalResolved {
			payload, err := model.DecodeStrict[transactionreducer.ApprovalResolvedPayload](event.Payload)
			if err != nil {
				return approvalAuthorityError(
					projection.Transaction.ID,
					"stored approval event is invalid",
				)
			}
			if payload.Decision.Digest == approvalDigest {
				continue
			}
			if sameAuthority(approver, payload.Decision.Approver) {
				return approvalAuthorityError(
					projection.Transaction.ID,
					"one identity cannot satisfy multiple approval decisions",
				)
			}
			continue
		}
		if sameAuthority(approver, event.Actor) {
			return approvalAuthorityError(
				projection.Transaction.ID,
				fmt.Sprintf(
					"approver also exercised transaction authority in event %q",
					event.Type,
				),
			)
		}
	}
	return nil
}

func validateReleaseSeparation(
	transactionID string,
	releaser model.Principal,
	decisions []model.ApprovalDecision,
) error {
	for index, decision := range decisions {
		if sameAuthority(releaser, decision.Approver) {
			return approvalAuthorityError(
				transactionID,
				"an approver cannot release the same transaction",
			)
		}
		for previous := 0; previous < index; previous++ {
			if sameAuthority(
				decision.Approver,
				decisions[previous].Approver,
			) {
				return approvalAuthorityError(
					transactionID,
					"one identity cannot satisfy multiple approval decisions",
				)
			}
		}
	}
	return nil
}

func approvedDecisions(
	projection transactionreducer.Projection,
	events []model.TransactionEvent,
) ([]model.ApprovalDecision, error) {
	if len(projection.ApprovalDigests) == 0 {
		return nil, nil
	}
	wanted := make(map[string]struct{}, len(projection.ApprovalDigests))
	for _, digest := range projection.ApprovalDigests {
		wanted[digest] = struct{}{}
	}
	found := make(map[string]model.ApprovalDecision, len(wanted))
	for _, event := range events {
		if event.Type != transactionreducer.EventApprovalResolved {
			continue
		}
		payload, err := model.DecodeStrict[transactionreducer.ApprovalResolvedPayload](
			event.Payload,
		)
		if err != nil {
			return nil, approvalAuthorityError(
				projection.Transaction.ID,
				"stored approval event is invalid",
			)
		}
		if _, required := wanted[payload.Decision.Digest]; required {
			found[payload.Decision.Digest] = payload.Decision
		}
	}
	decisions := make([]model.ApprovalDecision, 0, len(wanted))
	for _, digest := range projection.ApprovalDigests {
		decision, ok := found[digest]
		if !ok {
			return nil, approvalAuthorityError(
				projection.Transaction.ID,
				"approved decision is missing from the authoritative event history",
			)
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

func sameAuthority(left, right model.Principal) bool {
	if left.ID != "" && left.ID == right.ID {
		return true
	}
	return left.Issuer != "" &&
		left.Issuer == right.Issuer &&
		left.ClaimsDigest != "" &&
		left.ClaimsDigest == right.ClaimsDigest
}

func approvalAuthorityError(resource, message string) error {
	return &model.KernelError{
		Code:      model.ErrorApprovalInvalid,
		Operation: "authorize_transaction",
		Resource:  resource,
		Message:   message,
	}
}

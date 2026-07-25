package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/duriantaco/vouch/internal/kernel/model"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
	"github.com/duriantaco/vouch/internal/kernel/verification"
)

func TestApprovalSeparationRejectsSponsorAndPriorActor(t *testing.T) {
	sponsor := model.Principal{ID: "human:sponsor", Kind: model.PrincipalHuman}
	operator := model.Principal{
		ID: "operator:author", Kind: model.PrincipalOperator,
		Issuer: "https://identity.example/", ClaimsDigest: testDigest("b"),
	}
	projection := transactionreducer.Projection{
		Transaction: model.AgentTransaction{
			ID: "tx:authority", Sponsor: sponsor,
			AgentRunIDs: []string{"run:agent"},
		},
	}
	events := []model.TransactionEvent{{
		Type: transactionreducer.EventTransactionStaged, Actor: operator,
	}}

	if err := validateApprovalSeparation(projection, events, sponsor, ""); err == nil {
		t.Fatal("transaction sponsor approved its own transaction")
	}
	if err := validateApprovalSeparation(projection, events, operator, ""); err == nil {
		t.Fatal("prior transaction actor approved its own transaction")
	}
	if err := validateApprovalSeparation(
		projection,
		events,
		model.Principal{ID: "run:agent", Kind: model.PrincipalRun},
		"",
	); err == nil {
		t.Fatal("participating agent approved its own transaction")
	}
	if err := validateApprovalSeparation(
		projection,
		events,
		model.Principal{ID: "human:reviewer", Kind: model.PrincipalHuman},
		"",
	); err != nil {
		t.Fatalf("independent reviewer was rejected: %v", err)
	}
}

func TestApprovalSeparationMatchesVerifiedClaimsAcrossPrincipalAliases(t *testing.T) {
	claims := testDigest("c")
	events := []model.TransactionEvent{{
		Type: transactionreducer.EventCommitPlanFrozen,
		Actor: model.Principal{
			ID: "operator:author", Kind: model.PrincipalOperator,
			Issuer: "https://identity.example/", ClaimsDigest: claims,
		},
	}}
	approver := model.Principal{
		ID: "human:author", Kind: model.PrincipalHuman,
		Issuer: "https://identity.example/", ClaimsDigest: claims,
	}
	projection := transactionreducer.Projection{
		Transaction: model.AgentTransaction{
			ID: "tx:authority",
			Sponsor: model.Principal{
				ID: "human:sponsor", Kind: model.PrincipalHuman,
			},
		},
	}
	if err := validateApprovalSeparation(
		projection, events, approver, "",
	); err == nil {
		t.Fatal("principal aliases with the same verified claims bypassed separation of duty")
	}
}

func TestApprovalAndReleaseAuthoritiesMustBeDistinct(t *testing.T) {
	reviewer := model.Principal{
		ID: "human:reviewer", Kind: model.PrincipalHuman,
		Issuer: "https://identity.example/", ClaimsDigest: testDigest("e"),
	}
	otherReviewer := model.Principal{
		ID: "human:other-reviewer", Kind: model.PrincipalHuman,
		Issuer: "https://identity.example/", ClaimsDigest: testDigest("f"),
	}
	decisions := []model.ApprovalDecision{{
		Digest: testDigest("1"), Approver: reviewer,
	}}
	if err := validateReleaseSeparation(
		"tx:authority", reviewer, decisions,
	); err == nil {
		t.Fatal("approver released the same transaction")
	}
	if err := validateReleaseSeparation(
		"tx:authority",
		model.Principal{
			ID: "operator:release", Kind: model.PrincipalOperator,
			Issuer: "https://identity.example/", ClaimsDigest: testDigest("a"),
		},
		decisions,
	); err != nil {
		t.Fatalf("independent releaser was rejected: %v", err)
	}
	if err := validateReleaseSeparation(
		"tx:authority",
		model.Principal{
			ID: "operator:release", Kind: model.PrincipalOperator,
			Issuer: "https://identity.example/", ClaimsDigest: testDigest("a"),
		},
		append(decisions, model.ApprovalDecision{
			Digest: testDigest("2"), Approver: reviewer,
		}),
	); err == nil {
		t.Fatal("one reviewer satisfied multiple approval decisions")
	}
	if err := validateReleaseSeparation(
		"tx:authority",
		model.Principal{
			ID: "operator:release", Kind: model.PrincipalOperator,
			Issuer: "https://identity.example/", ClaimsDigest: testDigest("a"),
		},
		append(decisions, model.ApprovalDecision{
			Digest: testDigest("2"), Approver: otherReviewer,
		}),
	); err != nil {
		t.Fatalf("distinct reviewers were rejected: %v", err)
	}
}

func TestApprovedDecisionsComeFromAuthoritativeEventHistory(t *testing.T) {
	digest := testDigest("d")
	decision := model.ApprovalDecision{Digest: digest}
	payload, err := json.Marshal(transactionreducer.ApprovalResolvedPayload{
		Decision: decision,
	})
	if err != nil {
		t.Fatal(err)
	}
	projection := transactionreducer.Projection{ApprovalDigests: []string{digest}}
	decisions, err := approvedDecisions(projection, []model.TransactionEvent{{
		Type: transactionreducer.EventApprovalResolved, Payload: payload,
	}})
	if err != nil {
		t.Fatalf("read approved decision: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Digest != digest {
		t.Fatalf("unexpected approved decisions: %#v", decisions)
	}
	if _, err := approvedDecisions(projection, nil); err == nil {
		t.Fatal("missing authoritative approval event was accepted")
	}
}

func TestRequiredVerifierProfilesBindPreparationAndPolicyDigest(t *testing.T) {
	profiles := loadAuthorityProfiles(t, "verify", `["/verify","strict"]`)
	server := &Server{
		sequencePolicy:  transactionreducer.BaselinePolicy{},
		executionPolicy: testVerifierExecutionPolicy(profiles),
	}
	projection := transactionreducer.Projection{
		Transaction: model.AgentTransaction{
			ID:                "tx:profiles",
			EffectSetDigest:   testDigest("7"),
			StagedStateDigest: testDigest("8"),
		},
	}
	if err := server.validateVerifierProfiles(projection); err == nil {
		t.Fatal("missing required verifier profile was accepted")
	}
	profile, found := profiles.Lookup("verify")
	if !found {
		t.Fatal("test verifier profile is missing")
	}
	verifierDigest, err := server.expectedVerifierDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	inputsDigest, err := transactionreducer.ComputeVerificationInputsDigest(
		projection.Transaction.EffectSetDigest,
		projection.Transaction.StagedStateDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	projection.Verifications = []model.VerificationResult{{
		Name:              profile.Name,
		Status:            model.VerificationPassed,
		Independence:      model.VerificationPlatformRun,
		EffectSetDigest:   projection.Transaction.EffectSetDigest,
		StagedStateDigest: projection.Transaction.StagedStateDigest,
		VerifierDigest:    verifierDigest,
		InputsDigest:      inputsDigest,
		Verifier: model.Principal{
			ID: "service:vouch-verifier", Kind: model.PrincipalService,
			ClaimsDigest: profile.Digest,
		},
	}}
	if err := server.validateVerifierProfiles(projection); err != nil {
		t.Fatalf("current required verifier profile was rejected: %v", err)
	}
	firstDigest, err := server.currentAuthorityPolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	server.executionPolicy.VerifierProfiles = loadAuthorityProfiles(
		t, "verify", `["/verify","stricter"]`,
	)
	secondDigest, err := server.currentAuthorityPolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest {
		t.Fatal("verifier profile change did not invalidate authority policy digest")
	}
	if err := server.validateVerifierProfiles(projection); err == nil {
		t.Fatal("stale verifier result remained valid after profile change")
	}
	server.executionPolicy.VerifierProfiles = profiles
	server.executionPolicy.VerifierMemoryBytes = 8 << 30
	if err := server.validateVerifierProfiles(projection); err == nil {
		t.Fatal("stale verifier result remained valid after runtime policy change")
	}
}

func TestAuthorityPolicyBindsEnforcementAndTrustInputs(t *testing.T) {
	server := &Server{
		sequencePolicy: transactionreducer.BaselinePolicy{},
		executionPolicy: ExecutionRuntimePolicy{
			EnforcementProfile:  "development",
			IdentityTrustDigest: testDigest("1"),
			ApprovalTrustDigest: testDigest("a"),
		},
	}
	developmentDigest, err := server.currentAuthorityPolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	server.executionPolicy.EnforcementProfile = "production"
	productionDigest, err := server.currentAuthorityPolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	if developmentDigest == productionDigest {
		t.Fatal("runtime enforcement profile did not change authority policy")
	}
	server.executionPolicy.IdentityTrustDigest = testDigest("2")
	rotatedTrustDigest, err := server.currentAuthorityPolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	if productionDigest == rotatedTrustDigest {
		t.Fatal("identity trust change did not change authority policy")
	}
	server.executionPolicy.ApprovalTrustDigest = testDigest("b")
	rotatedApprovalTrustDigest, err := server.currentAuthorityPolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	if rotatedTrustDigest == rotatedApprovalTrustDigest {
		t.Fatal("approval trust change did not change authority policy")
	}
}

func testDigest(character string) string {
	value := ""
	for len(value) < 64 {
		value += character
	}
	return "sha256:" + value
}

func loadAuthorityProfiles(
	t *testing.T,
	name, commandJSON string,
) *verification.ProfileSet {
	t.Helper()
	path := filepath.Join(t.TempDir(), "verifier-profiles.json")
	document := `{
		"version":"vouch.verifier_profiles.v0",
		"profiles":[{
			"name":"` + name + `",
			"image":"registry.example/verifier@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"command":` + commandJSON + `,
			"timeout_seconds":300
		}]
	}`
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	profiles, err := verification.LoadProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	return profiles
}

func testVerifierExecutionPolicy(
	profiles *verification.ProfileSet,
) ExecutionRuntimePolicy {
	return ExecutionRuntimePolicy{
		VerifierProfiles:           profiles,
		RequireVerifierProfiles:    true,
		EnginePath:                 "/usr/bin/docker",
		VerifierUID:                1000,
		VerifierGID:                1000,
		VerifierMemoryBytes:        4 << 30,
		VerifierCPUMillis:          2000,
		VerifierPIDsLimit:          256,
		VerifierTmpfsBytes:         1 << 30,
		MaxConcurrentWorkloads:     2,
		MaxVerificationTimeoutSecs: 900,
	}
}

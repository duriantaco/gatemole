package gatemole

import (
	"os"
	"path/filepath"
)

func BuildArtifacts(specPath string, outDir string) error {
	spec, err := LoadJSON[Spec](specPath)
	if err != nil {
		return err
	}
	if errors := ValidateSpec(spec); len(errors) > 0 {
		return DiagnosticError{Diagnostics: stringDiagnostics("spec", errors)}
	}
	ir := IRFromSpec(spec)
	manifest := Manifest{
		Version: ManifestSchemaVersion,
		Change: Change{
			Risk:         spec.Risk,
			SpecsTouched: []string{spec.ID},
		},
		Runtime: ManifestRuntime{
			Metrics: append([]string(nil), spec.Runtime.Metrics...),
		},
		Rollback: ManifestRollback{
			Strategy: spec.Rollback.Strategy,
			Flag:     spec.Rollback.Flag,
		},
	}
	plan := VerificationPlanFromIR(ir, manifest)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(outDir, "verification-plan.json"), plan); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(outDir, "verifier-packets.json"), verifierPackets(ir)); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(outDir, "test-obligations.json"), testObligationsArtifact(ir)); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(outDir, "release-policy.json"), releasePolicyArtifact(ir)); err != nil {
		return err
	}
	return nil
}

func verifierPackets(ir IR) []VerifierPacket {
	var packets []VerifierPacket
	for _, role := range verifierRoles(ir) {
		packets = append(packets, VerifierPacket{
			Verifier:      role,
			Focus:         verifierFocus(role),
			PromptVersion: VerifierPromptVersion,
			OutputSchema:  VerifierOutputVersion,
			Obligations:   obligationsForVerifier(ir, role),
			RequiredOutput: "Return JSON matching " + VerifierOutputVersion + " with version, verifier, prompt_version, model, obligations, confidence, and findings. " +
				"Each finding must include verifier, decision, severity, claim, evidence, required_fix when blocking, and obligation IDs.",
		})
	}
	return packets
}

func verifierFocus(role string) string {
	switch role {
	case "spec_adherence":
		return "Check that implementation evidence traces to the declared feature obligations."
	case "test_adequacy":
		return "Check that required test obligations have concrete passing evidence."
	case "security":
		return "Check that security invariants have negative-test or security-check evidence."
	case "observability":
		return "Check that required runtime signals exist for rollout monitoring."
	case "rollback":
		return "Check that rollback or compensation evidence is sufficient for the risk."
	default:
		return "Check obligations assigned to this verifier."
	}
}

func obligationsForVerifier(ir IR, role string) []Obligation {
	var out []Obligation
	for _, obligation := range ir.Obligations {
		switch role {
		case "spec_adherence":
			if obligation.Kind == ObligationBehavior {
				out = append(out, obligation)
			}
		case "test_adequacy":
			if obligation.Kind == ObligationRequiredTest {
				out = append(out, obligation)
			}
		case "security":
			if obligation.Kind == ObligationSecurity {
				out = append(out, obligation)
			}
		case "observability":
			if obligation.Kind == ObligationRuntimeSignal {
				out = append(out, obligation)
			}
		case "rollback":
			if obligation.Kind == ObligationRollback {
				out = append(out, obligation)
			}
		}
	}
	return out
}

func testObligationsArtifact(ir IR) TestObligationsArtifact {
	return TestObligationsArtifact{
		Version:     PlanSchemaVersion,
		Feature:     ir.Feature,
		Obligations: obligationsForVerifier(ir, "test_adequacy"),
	}
}

func releasePolicyArtifact(ir IR) ReleasePolicyArtifact {
	return ReleasePolicyArtifact{
		Version:        PlanSchemaVersion,
		Feature:        ir.Feature,
		Risk:           ir.Risk,
		ReleasePolicy:  append([]string(nil), ir.ReleasePolicy...),
		RequiredChecks: append([]string(nil), ir.RequiredChecks...),
	}
}

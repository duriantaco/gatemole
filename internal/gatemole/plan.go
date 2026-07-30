package gatemole

import (
	"fmt"
)

func BuildVerificationPlanFile(specPath string, manifestPath string, outPath string) (VerificationPlan, error) {
	spec, err := LoadJSON[Spec](specPath)
	if err != nil {
		return VerificationPlan{}, err
	}
	if errors := ValidateSpec(spec); len(errors) > 0 {
		return VerificationPlan{}, DiagnosticError{Diagnostics: stringDiagnostics("spec", errors)}
	}
	manifest, err := LoadJSON[Manifest](manifestPath)
	if err != nil {
		return VerificationPlan{}, err
	}
	plan := VerificationPlanFromIR(IRFromSpec(spec), manifest)
	if err := writeJSONFile(outPath, plan); err != nil {
		return VerificationPlan{}, err
	}
	if HasErrorDiagnostics(plan.Diagnostics) {
		return plan, DiagnosticError{Diagnostics: plan.Diagnostics}
	}
	return plan, nil
}

func VerificationPlanFromIR(ir IR, manifest Manifest) VerificationPlan {
	diagnostics := []Diagnostic{}
	if len(manifest.Change.SpecsTouched) > 0 && !containsString(manifest.Change.SpecsTouched, ir.Feature) {
		diagnostics = append(diagnostics, Diagnostic{
			Severity: "warning",
			Code:     "plan.spec_not_touched",
			Message:  fmt.Sprintf("manifest does not list %s in change.specs_touched", ir.Feature),
			Path:     "change.specs_touched",
		})
	}
	specsTouched := append([]string(nil), manifest.Change.SpecsTouched...)
	if len(specsTouched) == 0 {
		specsTouched = []string{ir.Feature}
	}
	return VerificationPlan{
		Version:        PlanSchemaVersion,
		Feature:        ir.Feature,
		Risk:           ir.Risk,
		SpecsTouched:   specsTouched,
		Obligations:    append([]Obligation(nil), ir.Obligations...),
		RequiredChecks: append([]string(nil), ir.RequiredChecks...),
		VerifierRoles:  verifierRoles(ir),
		RuntimeSignals: append([]string(nil), ir.RuntimeSignals...),
		Rollback:       ir.Rollback,
		ReleasePolicy:  append([]string(nil), ir.ReleasePolicy...),
		Diagnostics:    diagnostics,
	}
}

func verifierRoles(ir IR) []string {
	roles := []string{"spec_adherence", "test_adequacy"}
	hasSecurity := false
	hasRuntime := false
	hasRollback := false
	for _, obligation := range ir.Obligations {
		switch obligation.Kind {
		case ObligationSecurity:
			hasSecurity = true
		case ObligationRuntimeSignal:
			hasRuntime = true
		case ObligationRollback:
			hasRollback = true
		}
	}
	if hasSecurity {
		roles = append(roles, "security")
	}
	if hasRuntime {
		roles = append(roles, "observability")
	}
	if hasRollback {
		roles = append(roles, "rollback")
	}
	return roles
}

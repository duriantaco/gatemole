package gatemole

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

type CompilerPipelineResult struct {
	Compilation       CompilationStats
	SpecErrors        []string
	SpecErrorsByID    map[string][]string
	ManifestErrors    []string
	Diagnostics       []Diagnostic
	Symbols           SymbolTable
	CompiledSymbols   SymbolTable
	IRs               map[string]IR
	VerificationPlans map[string]VerificationPlan
}

type SymbolTable struct {
	Specs       map[string]SpecSymbol
	Obligations map[string]ObligationSymbol
	FileOwners  []FileOwnerSymbol
}

type SpecSymbol struct {
	ID         string
	Risk       Risk
	OwnedPaths []string
}

type ObligationSymbol struct {
	ID               string
	SpecID           string
	Kind             ObligationKind
	RequiredEvidence EvidenceKind
}

type FileOwnerSymbol struct {
	SpecID  string
	Pattern string
}

func CompileManifestPipeline(specs map[string]Spec, manifest Manifest) CompilerPipelineResult {
	result := CompilerPipelineResult{
		Compilation:       CompilationStats{SpecsLoaded: len(specs)},
		SpecErrors:        []string{},
		SpecErrorsByID:    make(map[string][]string, len(specs)),
		ManifestErrors:    []string{},
		Diagnostics:       []Diagnostic{},
		Symbols:           NewSymbolTable(),
		CompiledSymbols:   NewSymbolTable(),
		IRs:               make(map[string]IR),
		VerificationPlans: make(map[string]VerificationPlan),
	}

	validSpecs := make(map[string]Spec, len(specs))
	for _, spec := range specs {
		errors := ValidateSpec(spec)
		result.SpecErrorsByID[spec.ID] = errors
		result.SpecErrors = append(result.SpecErrors, errors...)
		if len(errors) == 0 {
			validSpecs[spec.ID] = spec
		}
	}
	result.Symbols = BuildSymbolTable(validSpecs)

	compiledSpecIDs := make(map[string]bool, len(manifest.Change.SpecsTouched))
	for _, specID := range manifest.Change.SpecsTouched {
		if compiledSpecIDs[specID] {
			continue
		}
		compiledSpecIDs[specID] = true
		spec, ok := specs[specID]
		if !ok || len(result.SpecErrorsByID[specID]) > 0 {
			continue
		}
		ir := IRFromSpec(spec)
		plan := VerificationPlanFromIR(ir, manifest)
		result.IRs[spec.ID] = ir
		result.VerificationPlans[spec.ID] = plan
		result.Diagnostics = append(result.Diagnostics, plan.Diagnostics...)
		result.Compilation.SpecsCompiled++
		result.Compilation.ObligationsBuilt += len(ir.Obligations)
	}
	result.Compilation.SpecsSkipped = result.Compilation.SpecsLoaded - result.Compilation.SpecsCompiled
	result.CompiledSymbols = BuildPlanSymbolTable(result.VerificationPlans)
	result.ManifestErrors = append(result.ManifestErrors, ValidateManifest(manifest, specs)...)
	result.ManifestErrors = append(result.ManifestErrors, validateFileTraceability(manifest, result.Symbols)...)
	result.ManifestErrors = append(result.ManifestErrors, validateArtifactReferencesWithSymbols(manifest, result.CompiledSymbols)...)
	return result
}

func NewSymbolTable() SymbolTable {
	return SymbolTable{
		Specs:       make(map[string]SpecSymbol),
		Obligations: make(map[string]ObligationSymbol),
		FileOwners:  []FileOwnerSymbol{},
	}
}

func BuildSymbolTable(specs map[string]Spec) SymbolTable {
	table := NewSymbolTable()
	for _, specID := range sortedStringKeys(specs) {
		spec := specs[specID]
		table.Specs[spec.ID] = SpecSymbol{
			ID:         spec.ID,
			Risk:       spec.Risk,
			OwnedPaths: append([]string(nil), spec.OwnedPaths...),
		}
		for _, pattern := range spec.OwnedPaths {
			table.FileOwners = append(table.FileOwners, FileOwnerSymbol{
				SpecID:  spec.ID,
				Pattern: pattern,
			})
		}
		for _, obligation := range IRFromSpec(spec).Obligations {
			table.Obligations[obligation.ID] = ObligationSymbol{
				ID:               obligation.ID,
				SpecID:           spec.ID,
				Kind:             obligation.Kind,
				RequiredEvidence: obligation.RequiredEvidence,
			}
		}
	}
	sort.Slice(table.FileOwners, func(i, j int) bool {
		if table.FileOwners[i].SpecID == table.FileOwners[j].SpecID {
			return table.FileOwners[i].Pattern < table.FileOwners[j].Pattern
		}
		return table.FileOwners[i].SpecID < table.FileOwners[j].SpecID
	})
	return table
}

func BuildPlanSymbolTable(plans map[string]VerificationPlan) SymbolTable {
	table := NewSymbolTable()
	for _, specID := range sortedStringKeys(plans) {
		plan := plans[specID]
		table.Specs[plan.Feature] = SpecSymbol{ID: plan.Feature, Risk: plan.Risk}
		for _, obligation := range plan.Obligations {
			table.Obligations[obligation.ID] = ObligationSymbol{
				ID:               obligation.ID,
				SpecID:           plan.Feature,
				Kind:             obligation.Kind,
				RequiredEvidence: obligation.RequiredEvidence,
			}
		}
	}
	return table
}

func validateArtifactReferencesWithSymbols(manifest Manifest, symbols SymbolTable) []string {
	if len(manifest.Verification.Artifacts) == 0 {
		return nil
	}
	var errors []string
	for _, artifact := range manifest.Verification.Artifacts {
		for _, obligationID := range artifact.Obligations {
			obligation, ok := symbols.Obligations[obligationID]
			if !ok {
				errors = append(errors, fmt.Sprintf("manifest: verification.artifacts[%s] references unknown obligation %q", artifact.ID, obligationID))
				continue
			}
			if artifact.Kind != EvidenceVerifierOutput && artifact.Kind != obligation.RequiredEvidence {
				errors = append(errors, fmt.Sprintf("manifest: verification.artifacts[%s] kind %q does not satisfy obligation %s required evidence %q", artifact.ID, artifact.Kind, obligationID, obligation.RequiredEvidence))
			}
		}
	}
	return errors
}

func validateFileTraceability(manifest Manifest, symbols SymbolTable) []string {
	if len(manifest.Change.ChangedFiles) == 0 {
		return nil
	}
	touched := stringSet(manifest.Change.SpecsTouched)
	var errors []string
	for _, changedFile := range manifest.Change.ChangedFiles {
		normalized, ok := normalizeRepoPath(changedFile)
		if !ok {
			errors = append(errors, fmt.Sprintf("manifest: change.changed_files contains non-repo-relative path %q", changedFile))
			continue
		}
		owners := symbols.OwnersForFile(normalized)
		if len(owners) == 0 {
			errors = append(errors, fmt.Sprintf("manifest: changed file %q is not owned by any spec; add spec.owned_paths or update change.specs_touched", changedFile))
			continue
		}
		for _, specID := range owners {
			if !touched[specID] {
				errors = append(errors, fmt.Sprintf("manifest: changed file %q is owned by spec %s but that spec is not listed in change.specs_touched", changedFile, specID))
			}
		}
	}
	return errors
}

func (table SymbolTable) OwnersForFile(changedFile string) []string {
	seen := map[string]bool{}
	var owners []string
	for _, owner := range table.FileOwners {
		if ownedPathMatches(owner.Pattern, changedFile) && !seen[owner.SpecID] {
			seen[owner.SpecID] = true
			owners = append(owners, owner.SpecID)
		}
	}
	sort.Strings(owners)
	return owners
}

func ownedPathMatches(pattern string, changedFile string) bool {
	normalizedPattern, ok := normalizeRepoPath(pattern)
	if !ok {
		return false
	}
	normalizedFile, ok := normalizeRepoPath(changedFile)
	if !ok {
		return false
	}
	if strings.HasSuffix(normalizedPattern, "/**") {
		prefix := strings.TrimSuffix(normalizedPattern, "/**")
		return normalizedFile == prefix || strings.HasPrefix(normalizedFile, prefix+"/")
	}
	matched, err := path.Match(normalizedPattern, normalizedFile)
	return err == nil && matched
}

func normalizeRepoPath(value string) (string, bool) {
	if value == "" || filepath.IsAbs(value) {
		return "", false
	}
	normalized := filepath.ToSlash(filepath.Clean(value))
	normalized = strings.TrimPrefix(normalized, "./")
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") || strings.Contains(normalized, "/../") {
		return "", false
	}
	return normalized, true
}

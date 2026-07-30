package vouch

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func DefaultEvidenceManifest(repo string) string {
	return filepath.Join(repo, ".gatemole", "evidence", "manifest.json")
}

type EvidenceImportOptions struct {
	ArtifactPath string
	Out          string
}

func ImportJUnitEvidence(repo string, opts EvidenceImportOptions) (EvidenceImportResult, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return EvidenceImportResult{}, err
	}
	if opts.ArtifactPath == "" {
		return EvidenceImportResult{}, errors.New("evidence import junit requires a JUnit XML artifact path")
	}
	outPath := opts.Out
	if outPath == "" {
		outPath = DefaultEvidenceManifest(absRepo)
	} else {
		outPath = resolveRepoOutput(absRepo, outPath)
	}
	artifactPath, err := resolveArtifactPath(absRepo, opts.ArtifactPath)
	if err != nil {
		return EvidenceImportResult{}, err
	}
	artifactRel := repoRelativePath(absRepo, artifactPath)
	obligations, err := loadCompiledRequiredTests(absRepo)
	if err != nil {
		return EvidenceImportResult{}, err
	}
	data, err := os.ReadFile(artifactPath)
	if err != nil {
		return EvidenceImportResult{}, err
	}
	var root junitTestSuites
	if err := xml.Unmarshal(data, &root); err != nil {
		return EvidenceImportResult{}, fmt.Errorf("cannot parse JUnit XML: %w", err)
	}
	cases := collectJUnitCases(root)
	if len(cases) == 0 {
		return EvidenceImportResult{}, errors.New("JUnit XML contains no testcase elements")
	}

	links, unmatched := matchJUnitEvidenceLinks(obligations, cases, artifactRel)
	manifest := EvidenceManifest{
		Version:      EvidenceManifestVersion,
		ArtifactType: "junit",
		ArtifactPath: artifactRel,
		Links:        links,
	}
	if err := writeJSONFile(outPath, manifest); err != nil {
		return EvidenceImportResult{}, err
	}
	covered := make([]string, 0, len(links))
	for _, link := range links {
		if link.Status == "passed" {
			covered = append(covered, link.ObligationID)
		}
	}
	sort.Strings(covered)
	sort.Strings(unmatched)
	return EvidenceImportResult{
		Version:              EvidenceManifestVersion,
		InputPath:            artifactPath,
		OutputPath:           outPath,
		ArtifactType:         "junit",
		Links:                links,
		CoveredObligations:   covered,
		UnmatchedObligations: unmatched,
	}, nil
}

func CollectEvidenceFromEvidenceManifest(repo string, manifestPath string, opts CollectEvidenceOptions) (Evidence, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return Evidence{}, err
	}
	if manifestPath == "" {
		manifestPath = DefaultEvidenceManifest(absRepo)
	}
	absManifest := resolveRepoOutput(absRepo, manifestPath)
	evidenceManifest, err := LoadJSON[EvidenceManifest](absManifest)
	if err != nil {
		return Evidence{}, err
	}
	if evidenceManifest.Version != EvidenceManifestVersion {
		return Evidence{}, fmt.Errorf("evidence manifest version must be %s", EvidenceManifestVersion)
	}
	specs, err := LoadSpecs(absRepo)
	if err != nil {
		return Evidence{}, err
	}
	specIDs := sortedStringKeys(specs)
	if len(specIDs) == 0 {
		return Evidence{}, errors.New("no compiled specs found; run vouch compile first")
	}
	risk := maxSpecRisk(specs, specIDs)
	if risk == "" {
		risk = RiskMedium
	}
	manifest := Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "compiled-evidence", Summary: "gate compiled evidence manifest"},
		Change: Change{
			Risk:         risk,
			SpecsTouched: specIDs,
		},
		Agent: Agent{Name: "vouch"},
		Verification: Verification{
			Commands: []string{"vouch evidence import junit " + evidenceManifest.ArtifactPath},
		},
		Runtime: ManifestRuntime{
			Metrics: runtimeMetricsForSpecs(specs, specIDs),
			Canary: Canary{
				Enabled:        canaryInitialPercent(risk) > 0,
				InitialPercent: canaryInitialPercent(risk),
			},
		},
		Rollback: rollbackForSpecs(specs, specIDs),
	}
	pipeline := CompileManifestPipeline(specs, manifest)
	evidence := Evidence{
		Version:                EvidenceSchemaVersion,
		Repo:                   absRepo,
		ManifestPath:           absManifest,
		Compilation:            pipeline.Compilation,
		Manifest:               manifest,
		Specs:                  specs,
		IRs:                    pipeline.IRs,
		VerificationPlans:      pipeline.VerificationPlans,
		Diagnostics:            pipeline.Diagnostics,
		SignedEvidenceRequired: opts.RequireSigned,
		SpecErrors:             pipeline.SpecErrors,
		ManifestErrors:         pipeline.ManifestErrors,
		ArtifactResults:        artifactResultsFromEvidenceManifest(evidenceManifest),
		InvalidEvidence:        []InvalidEvidence{},
		VerifierOutputs:        []VerifierOutput{},
		RequiredObligations:    make(map[string][]Obligation),
		CoveredObligations:     make(map[string][]Obligation),
		MissingObligations:     make(map[string][]Obligation),
		RequiredTests:          make(map[string][]string),
		CoveredTests:           make(map[string][]string),
		MissingTests:           make(map[string][]string),
		RequiredSecurity:       make(map[string][]string),
		CoveredSecurity:        make(map[string][]string),
		MissingSecurity:        make(map[string][]string),
		Findings:               []Finding{},
		Reasons:                []string{},
	}
	buildCoverage(&evidence)
	runVerifiers(&evidence)
	importVerifierFindings(&evidence)
	policy, policyPath, err := LoadReleasePolicy(absRepo, opts.PolicyPath)
	if err != nil {
		return Evidence{}, err
	}
	ApplyReleasePolicy(&evidence, policy, policyPath)
	return evidence, nil
}

func loadCompiledRequiredTests(repo string) ([]Obligation, error) {
	path := filepath.Join(repo, ".gatemole", "build", "obligations.ir.json")
	bundle, err := LoadJSON[ObligationIRBundle](path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("compiled obligation IR not found at %s; run vouch compile first", path)
		}
		return nil, err
	}
	var obligations []Obligation
	for _, obligation := range bundle.Obligations {
		if obligation.Kind == ObligationRequiredTest {
			obligations = append(obligations, obligation)
		}
	}
	sort.Slice(obligations, func(i, j int) bool { return obligations[i].ID < obligations[j].ID })
	if len(obligations) == 0 {
		return nil, errors.New("compiled obligation IR contains no required-test obligations")
	}
	return obligations, nil
}

func matchJUnitEvidenceLinks(obligations []Obligation, cases []junitTestCase, artifactPath string) ([]EvidenceManifestLink, []string) {
	var links []EvidenceManifestLink
	var unmatched []string
	for _, obligation := range obligations {
		testCase, ok := bestJUnitMatch(obligation, cases)
		if !ok {
			unmatched = append(unmatched, obligation.ID)
			continue
		}
		links = append(links, EvidenceManifestLink{
			ObligationID:     obligation.ID,
			ArtifactType:     "junit",
			ArtifactPath:     artifactPath,
			Testcase:         junitEvidenceTestcase(testCase),
			Status:           junitEvidenceStatus(testCase),
			Component:        componentFromObligationID(obligation.ID),
			RequiredEvidence: obligation.RequiredEvidence,
		})
	}
	sort.Slice(links, func(i, j int) bool { return links[i].ObligationID < links[j].ObligationID })
	return links, unmatched
}

func bestJUnitMatch(obligation Obligation, cases []junitTestCase) (junitTestCase, bool) {
	bestScore := 0
	var best junitTestCase
	for _, testCase := range cases {
		score := junitMatchScore(obligation, testCase)
		if score > bestScore {
			bestScore = score
			best = testCase
		}
	}
	return best, bestScore > 0
}

func junitMatchScore(obligation Obligation, testCase junitTestCase) int {
	selectors := strings.ToLower(strings.Join(junitCaseSelectors(testCase), " "))
	score := 0
	if strings.Contains(selectors, strings.ToLower(obligation.ID)) {
		score += 100
	}
	if obligation.Generated != nil {
		sourceFile := strings.ToLower(filepath.ToSlash(obligation.Generated.Source.File))
		sourceSymbol := strings.ToLower(obligation.Generated.Source.Symbol)
		if sourceFile != "" && strings.Contains(selectors, strings.TrimSuffix(sourceFile, ".py")) {
			score += 60
		}
		if sourceSymbol != "" && strings.Contains(selectors, sourceSymbol) {
			score += 80
		}
	}
	if testCase.Name != "" && wordSet(testCase.Name)[obligationTailWord(obligation.ID)] {
		score += 30
	}
	score += 10 * sharedWordCount(obligationWords(obligation), wordSet(selectors))
	componentWords := wordSet(componentFromObligationID(obligation.ID))
	score += 5 * sharedWordCountFromSet(componentWords, wordSet(selectors))
	if junitEvidenceStatus(testCase) != "passed" {
		score -= 1
	}
	return score
}

func obligationWords(obligation Obligation) map[string]bool {
	words := wordSet(obligation.ID)
	for word := range wordSet(obligation.Text) {
		words[word] = true
	}
	return words
}

func wordSet(value string) map[string]bool {
	words := map[string]bool{}
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		word := b.String()
		if len(word) > 1 {
			words[word] = true
		}
		b.Reset()
	}
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return words
}

func sharedWordCount(left map[string]bool, right map[string]bool) int {
	return sharedWordCountFromSet(left, right)
}

func sharedWordCountFromSet(left map[string]bool, right map[string]bool) int {
	count := 0
	for word := range left {
		if right[word] {
			count++
		}
	}
	return count
}

func obligationTailWord(obligationID string) string {
	parts := strings.Split(obligationID, ".")
	if len(parts) == 0 {
		return ""
	}
	words := strings.FieldsFunc(parts[len(parts)-1], func(r rune) bool { return r == '_' || r == '-' })
	if len(words) == 0 {
		return parts[len(parts)-1]
	}
	return words[len(words)-1]
}

func componentFromObligationID(obligationID string) string {
	parts := strings.Split(obligationID, ".")
	if len(parts) < 3 {
		return obligationID
	}
	return strings.Join(parts[:len(parts)-2], ".")
}

func junitEvidenceStatus(testCase junitTestCase) string {
	switch {
	case testCase.Failure != nil || testCase.Error != nil:
		return "failed"
	case testCase.Skipped != nil:
		return "skipped"
	default:
		return "passed"
	}
}

func junitEvidenceTestcase(testCase junitTestCase) string {
	if testCase.File != "" && testCase.Name != "" {
		return filepath.ToSlash(testCase.File) + "::" + testCase.Name
	}
	if testCase.Classname != "" && testCase.Name != "" {
		return testCase.Classname + "::" + testCase.Name
	}
	return testCase.Label()
}

func artifactResultsFromEvidenceManifest(manifest EvidenceManifest) []ArtifactResult {
	result := ArtifactResult{
		ID:             manifest.ArtifactType,
		Kind:           EvidenceTestCoverage,
		Path:           manifest.ArtifactPath,
		ResolvedPath:   manifest.ArtifactPath,
		Status:         "valid",
		HashVerified:   false,
		BundleVerified: false,
	}
	seen := map[string]bool{}
	for _, link := range manifest.Links {
		if link.Status == "passed" {
			if !seen[link.ObligationID] {
				result.CoveredObligations = append(result.CoveredObligations, link.ObligationID)
				seen[link.ObligationID] = true
			}
			continue
		}
		result.FailedTests = append(result.FailedTests, link.Testcase)
	}
	sort.Strings(result.CoveredObligations)
	sort.Strings(result.FailedTests)
	return []ArtifactResult{result}
}

func shouldUseEvidenceManifestFallback(repo string, manifestPath string) bool {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return false
	}
	absManifest, err := filepath.Abs(manifestPath)
	if err != nil {
		return false
	}
	defaultManifest := DefaultManifest(absRepo)
	if absManifest != defaultManifest {
		return false
	}
	return !fileExists(absManifest) && fileExists(DefaultEvidenceManifest(absRepo))
}

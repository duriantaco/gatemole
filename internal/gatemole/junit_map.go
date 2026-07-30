package gatemole

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type JUnitMapOptions struct {
	ManifestPath string
	JUnitPath    string
	TestMapPath  string
	Out          string
}

func DefaultTestMap(repo string) string {
	return filepath.Join(repo, ".gatemole", "test-map.json")
}

func AppendTestMapStubs(repo string, spec Spec) error {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return err
	}
	path := DefaultTestMap(absRepo)
	testMap := TestMap{
		Version:  TestMapSchemaVersion,
		Mappings: map[string][]string{},
	}
	if fileExists(path) {
		loaded, err := LoadTestMap(path)
		if err != nil {
			return err
		}
		testMap = loaded
	}
	for _, obligation := range IRFromSpec(spec).Obligations {
		if obligation.Kind != ObligationRequiredTest {
			continue
		}
		if _, exists := testMap.Mappings[obligation.ID]; !exists {
			testMap.Mappings[obligation.ID] = []string{}
		}
	}
	return writeJSONFile(path, testMap)
}

func LoadTestMap(path string) (TestMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return TestMap{}, err
	}
	var versioned TestMap
	if err := json.Unmarshal(data, &versioned); err == nil && versioned.Mappings != nil {
		if versioned.Version != "" && versioned.Version != TestMapSchemaVersion {
			return TestMap{}, fmt.Errorf("%s: unsupported test map version %q", path, versioned.Version)
		}
		versioned.Version = TestMapSchemaVersion
		versioned.Mappings = normalizeTestMap(versioned.Mappings)
		return versioned, nil
	}
	var direct map[string][]string
	if err := json.Unmarshal(data, &direct); err != nil {
		return TestMap{}, fmt.Errorf("%s: %w", path, err)
	}
	return TestMap{
		Version:  TestMapSchemaVersion,
		Mappings: normalizeTestMap(direct),
	}, nil
}

func MapJUnitEvidence(repo string, opts JUnitMapOptions) (JUnitMapResult, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return JUnitMapResult{}, err
	}
	if opts.ManifestPath == "" || opts.JUnitPath == "" || opts.Out == "" {
		return JUnitMapResult{}, errors.New("junit map requires --manifest, --junit, and --out")
	}
	testMapPath := opts.TestMapPath
	if testMapPath == "" {
		testMapPath = DefaultTestMap(absRepo)
	}
	manifestPath := resolveRepoOutput(absRepo, opts.ManifestPath)
	junitPath, err := resolveArtifactPath(absRepo, opts.JUnitPath)
	if err != nil {
		return JUnitMapResult{}, err
	}
	resolvedTestMapPath := resolveRepoOutput(absRepo, testMapPath)
	outPath := resolveRepoOutput(absRepo, opts.Out)

	manifest, err := LoadJSON[Manifest](manifestPath)
	if err != nil {
		return JUnitMapResult{}, err
	}
	specs, err := LoadSpecs(absRepo)
	if err != nil {
		return JUnitMapResult{}, err
	}
	pipeline := CompileManifestPipeline(specs, manifest)
	if len(pipeline.SpecErrors) > 0 || len(pipeline.ManifestErrors) > 0 {
		return JUnitMapResult{}, fmt.Errorf("cannot map JUnit for invalid specs or manifest")
	}
	testMap, err := LoadTestMap(resolvedTestMapPath)
	if err != nil {
		return JUnitMapResult{}, err
	}
	data, err := os.ReadFile(junitPath)
	if err != nil {
		return JUnitMapResult{}, err
	}
	var root junitTestSuites
	if err := xml.Unmarshal(data, &root); err != nil {
		return JUnitMapResult{}, fmt.Errorf("cannot parse JUnit XML: %w", err)
	}
	cases := collectJUnitCases(root)
	if len(cases) == 0 {
		return JUnitMapResult{}, errors.New("JUnit XML contains no testcase elements")
	}
	required := requiredTestObligations(pipeline.VerificationPlans)
	mapped, err := mappedJUnitCases(required, cases, testMap)
	if err != nil {
		return JUnitMapResult{}, err
	}
	if err := writeMappedJUnit(outPath, mapped); err != nil {
		return JUnitMapResult{}, err
	}
	covered := make([]string, 0, len(mapped))
	for _, testCase := range mapped {
		covered = append(covered, testCase.Classname)
	}
	sort.Strings(covered)
	return JUnitMapResult{
		Version:            TestMapSchemaVersion,
		InputPath:          junitPath,
		OutputPath:         outPath,
		TestMapPath:        resolvedTestMapPath,
		Cases:              len(mapped),
		CoveredObligations: covered,
	}, nil
}

func requiredTestObligations(plans map[string]VerificationPlan) []Obligation {
	var obligations []Obligation
	for _, specID := range sortedStringKeys(plans) {
		for _, obligation := range plans[specID].Obligations {
			if obligation.Kind == ObligationRequiredTest {
				obligations = append(obligations, obligation)
			}
		}
	}
	return obligations
}

func mappedJUnitCases(required []Obligation, cases []junitTestCase, testMap TestMap) ([]junitTestCase, error) {
	passIndex := passingJUnitCaseIndex(cases)
	mapped := make([]junitTestCase, 0, len(required))
	var issues []string
	for _, obligation := range required {
		selectors := testMap.Mappings[obligation.ID]
		if len(selectors) == 0 {
			issues = append(issues, fmt.Sprintf("%s has no test-map selectors", obligation.ID))
			continue
		}
		if !selectorMatchesPassingCase(selectors, passIndex) {
			issues = append(issues, fmt.Sprintf("%s has no passing JUnit testcase matching %s", obligation.ID, strings.Join(selectors, ", ")))
			continue
		}
		mapped = append(mapped, junitTestCase{
			Classname: obligation.ID,
			Name:      obligation.Text,
		})
	}
	if len(issues) > 0 {
		return nil, errors.New(strings.Join(issues, "; "))
	}
	return mapped, nil
}

func passingJUnitCaseIndex(cases []junitTestCase) map[string]bool {
	index := map[string]bool{}
	for _, testCase := range cases {
		if testCase.Failure != nil || testCase.Error != nil || testCase.Skipped != nil {
			continue
		}
		for _, selector := range junitCaseSelectors(testCase) {
			index[selector] = true
		}
	}
	return index
}

func selectorMatchesPassingCase(selectors []string, index map[string]bool) bool {
	for _, selector := range selectors {
		selector = strings.TrimSpace(selector)
		if selector == "" {
			continue
		}
		for _, candidate := range selectorCandidates(selector) {
			if index[candidate] {
				return true
			}
		}
	}
	return false
}

func selectorCandidates(selector string) []string {
	selector = strings.TrimSpace(selector)
	selector = strings.TrimPrefix(selector, "./")
	candidates := []string{selector}
	if strings.Contains(selector, "::") {
		parts := strings.Split(selector, "::")
		if len(parts) >= 2 {
			module := strings.TrimSuffix(parts[0], ".py")
			module = strings.ReplaceAll(module, "/", ".")
			candidates = append(candidates, module+"."+parts[1])
		}
	}
	return uniqueStrings(candidates)
}

func junitCaseSelectors(testCase junitTestCase) []string {
	var selectors []string
	if testCase.Classname != "" {
		selectors = append(selectors, testCase.Classname)
	}
	if testCase.Name != "" {
		selectors = append(selectors, testCase.Name)
	}
	if label := testCase.Label(); label != "" {
		selectors = append(selectors, label)
	}
	if testCase.File != "" && testCase.Name != "" {
		file := filepath.ToSlash(testCase.File)
		selectors = append(selectors, file+"::"+testCase.Name)
		module := strings.TrimSuffix(file, ".py")
		module = strings.ReplaceAll(module, "/", ".")
		selectors = append(selectors, module+"."+testCase.Name)
	}
	if testCase.Classname != "" && testCase.Name != "" {
		pathSelector := strings.ReplaceAll(testCase.Classname, ".", "/") + ".py::" + testCase.Name
		selectors = append(selectors, pathSelector)
	}
	return uniqueStrings(selectors)
}

type mappedJUnitSuite struct {
	XMLName  xml.Name        `xml:"testsuite"`
	Name     string          `xml:"name,attr"`
	Tests    int             `xml:"tests,attr"`
	Failures int             `xml:"failures,attr"`
	Errors   int             `xml:"errors,attr"`
	Skipped  int             `xml:"skipped,attr"`
	Cases    []junitTestCase `xml:"testcase"`
}

func writeMappedJUnit(path string, cases []junitTestCase) error {
	var data bytes.Buffer
	data.WriteString(xml.Header)
	encoder := xml.NewEncoder(&data)
	encoder.Indent("", "  ")
	suite := mappedJUnitSuite{
		Name:     "vouch",
		Tests:    len(cases),
		Failures: 0,
		Errors:   0,
		Skipped:  0,
		Cases:    cases,
	}
	if err := encoder.Encode(suite); err != nil {
		return err
	}
	if err := encoder.Flush(); err != nil {
		return err
	}
	data.WriteByte('\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data.Bytes(), 0o644)
}

func normalizeTestMap(input map[string][]string) map[string][]string {
	out := make(map[string][]string, len(input))
	for obligationID, selectors := range input {
		out[obligationID] = uniqueStrings(selectors)
	}
	return out
}

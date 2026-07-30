package vouch

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestContractCreateWritesTestMapStubs(t *testing.T) {
	repo := initializedRepo(t)
	spec := createSampleContract(t, repo, RiskMedium)
	obligation := obligationID(t, spec, ObligationRequiredTest, "service json contract is stable")

	testMap, err := LoadTestMap(filepath.Join(repo, ".vouch", "test-map.json"))
	if err != nil {
		t.Fatal(err)
	}
	if testMap.Version != TestMapSchemaVersion {
		t.Fatalf("unexpected test map version %s", testMap.Version)
	}
	if selectors, ok := testMap.Mappings[obligation]; !ok || len(selectors) != 0 {
		t.Fatalf("expected empty test-map stub for %s, got %#v", obligation, testMap.Mappings)
	}
}

func TestJUnitMapConvertsRawPytestJUnitToObligationJUnit(t *testing.T) {
	repo, manifestPath, obligation := repoWithJUnitMapScenario(t)
	writeRawPytestJUnit(t, repo)

	result, err := MapJUnitEvidence(repo, JUnitMapOptions{
		ManifestPath: manifestPath,
		JUnitPath:    ".vouch/artifacts/pytest.xml",
		TestMapPath:  ".vouch/test-map.json",
		Out:          ".vouch/artifacts/vouch-junit.xml",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Cases != 1 || !contains(result.CoveredObligations, obligation) {
		t.Fatalf("expected mapped obligation %s, got %#v", obligation, result)
	}
	data := mustReadFile(t, filepath.Join(repo, ".vouch", "artifacts", "vouch-junit.xml"))
	if !bytes.Contains(data, []byte(`classname="`+obligation+`"`)) {
		t.Fatalf("mapped JUnit does not contain obligation id: %s", string(data))
	}
}

func TestAttachArtifactWithTestMapUsesMappedJUnit(t *testing.T) {
	repo, manifestPath, obligation := repoWithJUnitMapScenario(t)
	writeRawPytestJUnit(t, repo)

	updated, artifact, err := AttachArtifact(repo, AttachArtifactOptions{
		ManifestPath: manifestPath,
		ID:           "pytest",
		Kind:         EvidenceTestCoverage,
		Path:         ".vouch/artifacts/pytest.xml",
		TestMapPath:  ".vouch/test-map.json",
		Command:      "pytest --junitxml .vouch/artifacts/pytest.xml",
		ExitCode:     0,
		Out:          ".vouch/manifests/run.with-tests.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Path != ".vouch/artifacts/pytest-vouch-junit.xml" {
		t.Fatalf("expected mapped artifact path, got %s", artifact.Path)
	}
	if !contains(artifact.Obligations, obligation) {
		t.Fatalf("expected attached obligation %s, got %#v", obligation, artifact.Obligations)
	}
	if len(updated.Verification.Artifacts) != 1 {
		t.Fatalf("expected one attached artifact, got %#v", updated.Verification.Artifacts)
	}
}

func TestCLIJUnitMap(t *testing.T) {
	repo, manifestPath, obligation := repoWithJUnitMapScenario(t)
	writeRawPytestJUnit(t, repo)

	var stdout, stderr bytes.Buffer
	code := Main([]string{
		"--repo", repo,
		"junit", "map",
		"--manifest", manifestPath,
		"--junit", ".vouch/artifacts/pytest.xml",
		"--test-map", ".vouch/test-map.json",
		"--out", ".vouch/artifacts/vouch-junit.xml",
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("junit map failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), obligation) {
		t.Fatalf("expected JSON result to include obligation %s, got %s", obligation, stdout.String())
	}
}

func TestJUnitMapFailsWhenRequiredObligationHasNoSelector(t *testing.T) {
	repo, manifestPath, _ := repoWithJUnitMapScenario(t)
	writeRawPytestJUnit(t, repo)
	writeText(t, filepath.Join(repo, ".vouch", "test-map.json"), `{"version":"gatemole.test_map.v0","mappings":{}}`)

	_, err := MapJUnitEvidence(repo, JUnitMapOptions{
		ManifestPath: manifestPath,
		JUnitPath:    ".vouch/artifacts/pytest.xml",
		TestMapPath:  ".vouch/test-map.json",
		Out:          ".vouch/artifacts/vouch-junit.xml",
	})
	if err == nil || !strings.Contains(err.Error(), "has no test-map selectors") {
		t.Fatalf("expected missing selector error, got %v", err)
	}
}

func repoWithJUnitMapScenario(t *testing.T) (string, string, string) {
	t.Helper()
	repo := initializedRepo(t)
	spec := createSampleContract(t, repo, RiskMedium)
	obligation := obligationID(t, spec, ObligationRequiredTest, "service json contract is stable")
	_, err := CreateManifest(repo, ManifestCreateOptions{
		TaskID:       "agent-1",
		Summary:      "change app service",
		Agent:        "codex",
		RunID:        "run-1",
		ChangedFiles: []string{"src/app/service.py"},
		Out:          ".vouch/manifests/run.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	writeText(t, filepath.Join(repo, ".vouch", "test-map.json"), `{
  "version": "gatemole.test_map.v0",
  "mappings": {
    "`+obligation+`": [
      "tests/test_app.py::test_service_json_contract"
    ]
  }
}`)
	return repo, ".vouch/manifests/run.json", obligation
}

func writeRawPytestJUnit(t *testing.T, repo string) {
	t.Helper()
	writeText(t, filepath.Join(repo, ".vouch", "artifacts", "pytest.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="pytest" tests="1" failures="0" errors="0" skipped="0">
  <testcase classname="tests.test_app" name="test_service_json_contract" file="tests/test_app.py" />
</testsuite>
`)
}

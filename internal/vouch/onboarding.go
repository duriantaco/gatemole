package vouch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var contractNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type stringListFlag []string

func (f *stringListFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(value string) error {
	for _, item := range strings.Split(value, ",") {
		trimmed := strings.TrimSpace(item)
		if trimmed != "" {
			*f = append(*f, trimmed)
		}
	}
	return nil
}

func DefaultConfig(repo string) Config {
	profiles := DetectProfiles(repo)
	commands := DetectCommands(repo, profiles)
	return Config{
		Version:        ConfigSchemaVersion,
		Profiles:       profiles,
		Commands:       commands,
		ArtifactDir:    ".gatemole/artifacts",
		ManifestDir:    ".gatemole/manifests",
		BuildDir:       ".gatemole/build",
		AllowedSigners: []AllowedSigner{},
		IgnoredPaths: []string{
			".git/**",
			".gatemole/artifacts/**",
			".gatemole/build/**",
			"node_modules/**",
			"vendor/**",
			"dist/**",
			"build/**",
			"target/**",
		},
	}
}

func InitRepo(repo string, profileOverride string, force bool) (InitResult, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return InitResult{}, err
	}
	config := DefaultConfig(absRepo)
	if profileOverride != "" && profileOverride != "auto" {
		if !supportedProfile(profileOverride) {
			return InitResult{}, fmt.Errorf("unsupported profile %q", profileOverride)
		}
		config.Profiles = []string{profileOverride}
		config.Commands = DetectCommands(absRepo, config.Profiles)
	}
	dirs := []string{
		".gatemole",
		".gatemole/intents",
		".gatemole/specs",
		".gatemole/policy",
		config.ManifestDir,
		config.ArtifactDir,
		config.BuildDir,
	}
	var createdDirs []string
	for _, dir := range dirs {
		path := filepath.Join(absRepo, filepath.FromSlash(dir))
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			createdDirs = append(createdDirs, dir)
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return InitResult{}, err
		}
	}
	configPath := filepath.Join(absRepo, ".gatemole", "config.json")
	created := false
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) || force {
		if err := writeJSONFile(configPath, config); err != nil {
			return InitResult{}, err
		}
		created = true
	} else if err != nil {
		return InitResult{}, err
	}
	policyPath := DefaultPolicyPath(absRepo)
	if _, err := os.Stat(policyPath); errors.Is(err, os.ErrNotExist) || force {
		if err := WriteDefaultReleasePolicy(policyPath); err != nil {
			return InitResult{}, err
		}
	} else if err != nil {
		return InitResult{}, err
	}
	return InitResult{
		Version:     ConfigSchemaVersion,
		Repo:        absRepo,
		ConfigPath:  configPath,
		Profiles:    append([]string(nil), config.Profiles...),
		Commands:    append([]string(nil), config.Commands...),
		CreatedDirs: createdDirs,
		Created:     created,
	}, nil
}

func LoadConfigOrDefault(repo string) Config {
	configPath := filepath.Join(repo, ".gatemole", "config.json")
	config, err := LoadJSON[Config](configPath)
	if err == nil && config.Version == ConfigSchemaVersion {
		return config
	}
	return DefaultConfig(repo)
}

func DetectProfiles(repo string) []string {
	candidates := []struct {
		name  string
		files []string
	}{
		{"python", []string{"pyproject.toml", "setup.py", "requirements.txt"}},
		{"node", []string{"package.json"}},
		{"go", []string{"go.mod"}},
		{"rust", []string{"Cargo.toml"}},
	}
	var profiles []string
	for _, candidate := range candidates {
		for _, file := range candidate.files {
			if fileExists(filepath.Join(repo, file)) {
				profiles = append(profiles, candidate.name)
				break
			}
		}
	}
	if len(profiles) == 0 {
		return []string{"generic"}
	}
	return profiles
}

func DetectCommands(repo string, profiles []string) []string {
	var commands []string
	for _, profile := range profiles {
		switch profile {
		case "python":
			commands = append(commands, detectPythonCommands(repo)...)
		case "node":
			commands = append(commands, detectNodeCommands(repo)...)
		case "go":
			commands = append(commands, "go test ./...")
		case "rust":
			commands = append(commands, "cargo test", "cargo clippy --all-targets --all-features")
		}
	}
	commands = uniqueStrings(commands)
	if len(commands) == 0 {
		return []string{"manual verification required"}
	}
	return commands
}

func ContractSuggestions(repo string) ([]ContractSuggestion, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	profiles := DetectProfiles(absRepo)
	suggestions := []ContractSuggestion{}
	for _, profile := range profiles {
		switch profile {
		case "python":
			suggestions = append(suggestions, pythonSuggestions(absRepo)...)
		case "node":
			suggestions = append(suggestions, directorySuggestions(absRepo, "node", "src", "tests")...)
		case "go":
			suggestions = append(suggestions, goSuggestions(absRepo)...)
		case "rust":
			suggestions = append(suggestions, rustSuggestions(absRepo)...)
		default:
			suggestions = append(suggestions, genericSuggestions(absRepo)...)
		}
	}
	sort.Slice(suggestions, func(i, j int) bool {
		if suggestions[i].Profile == suggestions[j].Profile {
			return suggestions[i].Name < suggestions[j].Name
		}
		return suggestions[i].Profile < suggestions[j].Profile
	})
	return suggestions, nil
}

func CreateContract(repo string, intent Intent, force bool) (Spec, string, string, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return Spec{}, "", "", err
	}
	if err := validateContractName(intent.Feature); err != nil {
		return Spec{}, "", "", err
	}
	if len(intent.Security) == 0 {
		intent.Security = []string{"no owned-path changes bypass this contract"}
	}
	if len(intent.RuntimeMetrics) == 0 {
		intent.RuntimeMetrics = []string{"gatemole.gate.decision"}
	}
	if intent.Rollback.Strategy == "" {
		intent.Rollback.Strategy = "revert_change"
	}
	if len(intent.OwnedPaths) == 0 {
		return Spec{}, "", "", errors.New("contract requires at least one owned path")
	}
	if diagnostics := ValidateIntentDiagnostics(intent); HasErrorDiagnostics(diagnostics) {
		return Spec{}, "", "", DiagnosticError{Diagnostics: diagnostics}
	}
	specs, err := LoadSpecs(absRepo)
	if err != nil {
		return Spec{}, "", "", err
	}
	if !force {
		if _, exists := specs[intent.Feature]; exists {
			return Spec{}, "", "", fmt.Errorf("spec %q already exists", intent.Feature)
		}
	}
	fileName := intent.Feature + ".yaml"
	intentPath := filepath.Join(absRepo, ".gatemole", "intents", fileName)
	specPath := filepath.Join(absRepo, ".gatemole", "specs", intent.Feature+".json")
	if !force {
		if fileExists(intentPath) {
			return Spec{}, "", "", fmt.Errorf("intent already exists: %s", intentPath)
		}
		if fileExists(specPath) {
			return Spec{}, "", "", fmt.Errorf("spec already exists: %s", specPath)
		}
	}
	if err := os.MkdirAll(filepath.Dir(intentPath), 0o755); err != nil {
		return Spec{}, "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(specPath), 0o755); err != nil {
		return Spec{}, "", "", err
	}
	if err := os.WriteFile(intentPath, []byte(renderIntentYAML(intent)), 0o644); err != nil {
		return Spec{}, "", "", err
	}
	spec, err := CompileIntentFile(intentPath, specPath)
	if err != nil {
		return Spec{}, "", "", err
	}
	if err := AppendTestMapStubs(absRepo, spec); err != nil {
		return Spec{}, "", "", err
	}
	return spec, intentPath, specPath, nil
}

type ManifestCreateOptions struct {
	TaskID           string
	Summary          string
	Agent            string
	RunID            string
	Model            string
	RunnerIdentity   string
	RunnerOIDCIssuer string
	Base             string
	Head             string
	Risk             Risk
	ChangedFiles     []string
	ExternalEffects  []string
	MigrationChanged bool
	Out              string
}

func CreateManifest(repo string, opts ManifestCreateOptions) (Manifest, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return Manifest{}, err
	}
	if opts.TaskID == "" || opts.Summary == "" || opts.Agent == "" || opts.RunID == "" || opts.Out == "" {
		return Manifest{}, errors.New("manifest create requires --task-id, --summary, --agent, --run-id, and --out")
	}
	specs, err := LoadSpecs(absRepo)
	if err != nil {
		return Manifest{}, err
	}
	if len(specs) == 0 {
		return Manifest{}, errors.New("manifest create requires at least one spec")
	}
	changedFiles := append([]string(nil), opts.ChangedFiles...)
	if len(changedFiles) == 0 {
		changedFiles, err = gitChangedFiles(absRepo, opts.Base, opts.Head)
		if err != nil {
			return Manifest{}, err
		}
	}
	changedFiles = normalizeChangedFiles(changedFiles)
	symbols := BuildSymbolTable(validSpecMap(specs))
	specsTouched := specsForChangedFiles(symbols, changedFiles)
	maxRisk := maxSpecRisk(specs, specsTouched)
	risk := maxRisk
	if opts.Risk != "" {
		if !validRisk(opts.Risk) {
			return Manifest{}, fmt.Errorf("invalid risk %q", opts.Risk)
		}
		if validRisk(maxRisk) && riskRank[opts.Risk] < riskRank[maxRisk] {
			return Manifest{}, fmt.Errorf("manifest risk %q cannot be lower than touched spec risk %q", opts.Risk, maxRisk)
		}
		risk = opts.Risk
	}
	if risk == "" {
		risk = RiskLow
	}
	config := LoadConfigOrDefault(absRepo)
	runtimeMetrics := runtimeMetricsForSpecs(specs, specsTouched)
	rollback := rollbackForSpecs(specs, specsTouched)
	manifest := Manifest{
		Version: ManifestSchemaVersion,
		Task: Task{
			ID:      opts.TaskID,
			Summary: opts.Summary,
		},
		Change: Change{
			Risk:             risk,
			SpecsTouched:     specsTouched,
			BehaviorChanged:  len(specsTouched) > 0,
			MigrationChanged: opts.MigrationChanged,
			ExternalEffects:  uniqueStrings(opts.ExternalEffects),
			ChangedFiles:     changedFiles,
		},
		Agent: Agent{
			Name:             opts.Agent,
			RunID:            opts.RunID,
			Model:            opts.Model,
			RunnerIdentity:   opts.RunnerIdentity,
			RunnerOIDCIssuer: opts.RunnerOIDCIssuer,
		},
		Verification: Verification{
			CoversBehavior: []string{},
			Commands:       append([]string(nil), config.Commands...),
			TestsAdded:     []string{},
			CoversTests:    []string{},
			CoversSecurity: []string{},
			Artifacts:      []EvidenceArtifact{},
			TestResults: TestResults{
				Passed: 0,
				Failed: 0,
			},
		},
		Runtime: ManifestRuntime{
			Metrics: runtimeMetrics,
			Canary: Canary{
				Enabled:        riskRank[risk] >= riskRank[RiskHigh],
				InitialPercent: canaryInitialPercent(risk),
			},
		},
		Rollback:      rollback,
		Uncertainties: []string{},
	}
	outPath := resolveRepoOutput(absRepo, opts.Out)
	if err := writeJSONFile(outPath, manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

type AttachArtifactOptions struct {
	ManifestPath     string
	ID               string
	Kind             EvidenceKind
	Path             string
	TestMapPath      string
	Producer         string
	Command          string
	ExitCode         int
	SHA256           string
	EvidenceBundle   string
	SignatureBundle  string
	SignerIdentity   string
	SignerOIDCIssuer string
	Out              string
}

func AttachArtifact(repo string, opts AttachArtifactOptions) (Manifest, EvidenceArtifact, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return Manifest{}, EvidenceArtifact{}, err
	}
	if opts.ManifestPath == "" || opts.ID == "" || opts.Kind == "" || opts.Path == "" || opts.Out == "" {
		return Manifest{}, EvidenceArtifact{}, errors.New("attach-artifact requires --manifest, --id, --kind, --path, and --out")
	}
	if !validEvidenceKind(opts.Kind) {
		return Manifest{}, EvidenceArtifact{}, fmt.Errorf("unsupported artifact kind %q", opts.Kind)
	}
	if opts.ExitCode != 0 {
		return Manifest{}, EvidenceArtifact{}, fmt.Errorf("artifact exit code must be 0, got %d", opts.ExitCode)
	}
	if err := validateSignatureMetadata(opts.EvidenceBundle, opts.SignatureBundle, opts.SignerIdentity, opts.SignerOIDCIssuer); err != nil {
		return Manifest{}, EvidenceArtifact{}, err
	}
	artifactPath := opts.Path
	if opts.TestMapPath != "" {
		if opts.Kind != EvidenceTestCoverage {
			return Manifest{}, EvidenceArtifact{}, errors.New("--test-map is only supported for test_coverage artifacts")
		}
		if opts.SHA256 != "" {
			return Manifest{}, EvidenceArtifact{}, errors.New("--sha256 cannot be combined with --test-map because the mapped JUnit artifact is generated")
		}
		if opts.EvidenceBundle != "" || opts.SignatureBundle != "" || opts.SignerIdentity != "" || opts.SignerOIDCIssuer != "" {
			return Manifest{}, EvidenceArtifact{}, errors.New("signature metadata cannot be combined with --test-map because the mapped JUnit artifact is generated")
		}
		mappedPath := defaultMappedJUnitArtifactPath(opts.ID)
		if _, err := MapJUnitEvidence(absRepo, JUnitMapOptions{
			ManifestPath: opts.ManifestPath,
			JUnitPath:    opts.Path,
			TestMapPath:  opts.TestMapPath,
			Out:          mappedPath,
		}); err != nil {
			return Manifest{}, EvidenceArtifact{}, err
		}
		artifactPath = mappedPath
	}
	resolved, err := resolveArtifactPath(absRepo, artifactPath)
	if err != nil {
		return Manifest{}, EvidenceArtifact{}, err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return Manifest{}, EvidenceArtifact{}, err
	}
	manifestPath := resolveRepoOutput(absRepo, opts.ManifestPath)
	manifest, err := LoadJSON[Manifest](manifestPath)
	if err != nil {
		return Manifest{}, EvidenceArtifact{}, err
	}
	for _, artifact := range manifest.Verification.Artifacts {
		if artifact.ID == opts.ID {
			return Manifest{}, EvidenceArtifact{}, fmt.Errorf("manifest already contains artifact id %q", opts.ID)
		}
	}
	specs, err := LoadSpecs(absRepo)
	if err != nil {
		return Manifest{}, EvidenceArtifact{}, err
	}
	pipeline := CompileManifestPipeline(specs, manifest)
	candidates := obligationsForEvidenceKind(pipeline.CompiledSymbols, opts.Kind)
	if len(candidates) == 0 {
		return Manifest{}, EvidenceArtifact{}, fmt.Errorf("manifest has no compiled obligations requiring %q evidence", opts.Kind)
	}
	covered, issues := importArtifactCoverage(data, opts.Kind, candidates)
	if len(covered) == 0 {
		return Manifest{}, EvidenceArtifact{}, fmt.Errorf("artifact covers no %q obligations: %s", opts.Kind, strings.Join(issues, "; "))
	}
	artifactSHA256 := opts.SHA256
	if artifactSHA256 == "" {
		artifactSHA256 = sha256Hex(data)
	}
	producer := opts.Producer
	if producer == "" {
		producer = manifest.Agent.Name
	}
	if producer == "" {
		producer = "local"
	}
	artifact := EvidenceArtifact{
		ID:               opts.ID,
		Kind:             opts.Kind,
		Producer:         producer,
		Command:          opts.Command,
		Path:             artifactPath,
		SHA256:           artifactSHA256,
		EvidenceBundle:   opts.EvidenceBundle,
		SignatureBundle:  opts.SignatureBundle,
		SignerIdentity:   opts.SignerIdentity,
		SignerOIDCIssuer: opts.SignerOIDCIssuer,
		ExitCode:         &opts.ExitCode,
		Obligations:      covered,
	}
	manifest.Verification.Artifacts = append(manifest.Verification.Artifacts, artifact)
	sort.Slice(manifest.Verification.Artifacts, func(i, j int) bool {
		return manifest.Verification.Artifacts[i].ID < manifest.Verification.Artifacts[j].ID
	})
	outPath := resolveRepoOutput(absRepo, opts.Out)
	if err := writeJSONFile(outPath, manifest); err != nil {
		return Manifest{}, EvidenceArtifact{}, err
	}
	return manifest, artifact, nil
}

func validateSignatureMetadata(evidenceBundle string, signatureBundle string, identity string, issuer string) error {
	if evidenceBundle == "" && signatureBundle == "" && identity == "" && issuer == "" {
		return nil
	}
	if evidenceBundle == "" || signatureBundle == "" || identity == "" || issuer == "" {
		return errors.New("signed artifacts require --evidence-bundle, --signature-bundle, --signer-identity, and --signer-oidc-issuer together")
	}
	return nil
}

func defaultMappedJUnitArtifactPath(artifactID string) string {
	name := sanitizeContractName(artifactID)
	name = strings.ReplaceAll(name, ".", "-")
	if name == "" {
		name = "test"
	}
	return filepath.ToSlash(filepath.Join(".gatemole", "artifacts", name+"-vouch-junit.xml"))
}

func supportedProfile(profile string) bool {
	switch profile {
	case "python", "node", "go", "rust", "generic":
		return true
	default:
		return false
	}
}

func detectPythonCommands(repo string) []string {
	pyprojectPath := filepath.Join(repo, "pyproject.toml")
	data, _ := os.ReadFile(pyprojectPath)
	text := string(data)
	var commands []string
	if strings.Contains(text, "[tool.fyn.tasks]") || fileExists(filepath.Join(repo, "fyn.lock")) {
		for _, task := range []string{"lint", "format-check", "typecheck"} {
			if strings.Contains(text, task) {
				commands = append(commands, "fyn run "+task)
			}
		}
		if strings.Contains(text, "pytest") || dirExists(filepath.Join(repo, "tests")) {
			commands = append(commands, "fyn run pytest --junitxml .gatemole/artifacts/junit.xml")
		}
		return commands
	}
	if strings.Contains(text, "ruff") {
		commands = append(commands, "ruff check .")
	}
	if strings.Contains(text, "mypy") {
		commands = append(commands, "mypy src")
	}
	if strings.Contains(text, "pytest") || dirExists(filepath.Join(repo, "tests")) {
		commands = append(commands, "pytest --junitxml .gatemole/artifacts/junit.xml")
	}
	return commands
}

func detectNodeCommands(repo string) []string {
	type packageJSON struct {
		Scripts map[string]string `json:"scripts"`
	}
	var pkg packageJSON
	data, err := os.ReadFile(filepath.Join(repo, "package.json"))
	if err != nil || json.Unmarshal(data, &pkg) != nil {
		return []string{"npm test"}
	}
	var commands []string
	for _, name := range []string{"lint", "typecheck", "test"} {
		if _, ok := pkg.Scripts[name]; ok {
			commands = append(commands, "npm run "+name)
		}
	}
	return commands
}

func pythonSuggestions(repo string) []ContractSuggestion {
	var suggestions []ContractSuggestion
	pkgs := pythonPackageDirs(repo)
	for _, pkg := range pkgs {
		pkgDir := filepath.Join(repo, filepath.FromSlash(pkg.RelPath))
		if dirHasFileSuffix(pkgDir, ".py") {
			suggestions = append(suggestions, ContractSuggestion{
				Name:       rootPythonSuggestionName(pkg.Name, pkgDir),
				Profile:    "python",
				OwnedPaths: []string{filepath.ToSlash(filepath.Join(pkg.RelPath, "*.py")), "tests/**"},
				TestPaths:  []string{"tests/**"},
				Confidence: "low",
			})
		}
		for _, module := range childDirsWithFiles(pkgDir, ".py") {
			name := sanitizeContractName(pkg.Name + "." + module)
			testPattern := "tests/test_" + strings.ReplaceAll(module, "-", "_") + "*.py"
			suggestions = append(suggestions, ContractSuggestion{
				Name:       name,
				Profile:    "python",
				OwnedPaths: []string{filepath.ToSlash(filepath.Join(pkg.RelPath, module, "**")), testPattern},
				TestPaths:  []string{testPattern},
				Confidence: "medium",
			})
		}
		if len(suggestions) == 0 {
			suggestions = append(suggestions, ContractSuggestion{
				Name:       sanitizeContractName(pkg.Name + ".core"),
				Profile:    "python",
				OwnedPaths: []string{filepath.ToSlash(filepath.Join(pkg.RelPath, "**")), "tests/**"},
				TestPaths:  []string{"tests/**"},
				Confidence: "low",
			})
		}
	}
	if len(suggestions) == 0 {
		return genericSuggestions(repo)
	}
	return suggestions
}

func rootPythonSuggestionName(pkgName string, pkgDir string) string {
	if dirExists(filepath.Join(pkgDir, "core")) {
		return sanitizeContractName(pkgName + ".package")
	}
	return sanitizeContractName(pkgName + ".core")
}

func directorySuggestions(repo string, profile string, sourceDir string, testDir string) []ContractSuggestion {
	var suggestions []ContractSuggestion
	for _, dir := range childDirs(filepath.Join(repo, sourceDir)) {
		suggestions = append(suggestions, ContractSuggestion{
			Name:       sanitizeContractName(dir),
			Profile:    profile,
			OwnedPaths: []string{filepath.ToSlash(filepath.Join(sourceDir, dir, "**")), filepath.ToSlash(filepath.Join(testDir, dir, "**"))},
			TestPaths:  []string{filepath.ToSlash(filepath.Join(testDir, dir, "**"))},
			Confidence: "low",
		})
	}
	if len(suggestions) == 0 && dirExists(filepath.Join(repo, sourceDir)) {
		suggestions = append(suggestions, ContractSuggestion{
			Name:       "project.core",
			Profile:    profile,
			OwnedPaths: []string{sourceDir + "/**", testDir + "/**"},
			TestPaths:  []string{testDir + "/**"},
			Confidence: "low",
		})
	}
	return suggestions
}

func goSuggestions(repo string) []ContractSuggestion {
	var suggestions []ContractSuggestion
	_ = filepath.WalkDir(repo, func(path string, entry os.DirEntry, err error) error {
		if err != nil || !entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if name == ".git" ||
			name == ".gatemole" ||
			name == ".vouch" ||
			name == "vendor" {
			return filepath.SkipDir
		}
		if path == repo || !dirHasFileSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(repo, path)
		if err != nil {
			return nil
		}
		suggestions = append(suggestions, ContractSuggestion{
			Name:       sanitizeContractName(strings.ReplaceAll(filepath.ToSlash(rel), "/", ".")),
			Profile:    "go",
			OwnedPaths: []string{filepath.ToSlash(filepath.Join(rel, "**"))},
			TestPaths:  []string{filepath.ToSlash(filepath.Join(rel, "*_test.go"))},
			Confidence: "low",
		})
		return nil
	})
	return suggestions
}

func rustSuggestions(repo string) []ContractSuggestion {
	if dirExists(filepath.Join(repo, "crates")) {
		return directorySuggestions(repo, "rust", "crates", "tests")
	}
	if dirExists(filepath.Join(repo, "src")) {
		return []ContractSuggestion{{
			Name:       "crate.core",
			Profile:    "rust",
			OwnedPaths: []string{"src/**", "tests/**"},
			TestPaths:  []string{"tests/**"},
			Confidence: "low",
		}}
	}
	return nil
}

func genericSuggestions(repo string) []ContractSuggestion {
	if dirExists(filepath.Join(repo, "src")) {
		return []ContractSuggestion{{
			Name:       "project.core",
			Profile:    "generic",
			OwnedPaths: []string{"src/**", "tests/**"},
			TestPaths:  []string{"tests/**"},
			Confidence: "low",
		}}
	}
	return nil
}

type pythonPackageDir struct {
	Name    string
	RelPath string
}

func pythonPackageDirs(repo string) []pythonPackageDir {
	var packages []pythonPackageDir
	src := filepath.Join(repo, "src")
	for _, dir := range childDirs(src) {
		if fileExists(filepath.Join(src, dir, "__init__.py")) || dirHasFileSuffix(filepath.Join(src, dir), ".py") {
			packages = append(packages, pythonPackageDir{Name: dir, RelPath: filepath.ToSlash(filepath.Join("src", dir))})
		}
	}
	for _, dir := range childDirs(repo) {
		if dir == "src" || dir == "tests" || strings.HasPrefix(dir, ".") {
			continue
		}
		if fileExists(filepath.Join(repo, dir, "__init__.py")) {
			packages = append(packages, pythonPackageDir{Name: dir, RelPath: dir})
		}
	}
	return packages
}

func childDirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && entry.Name() != "__pycache__" {
			dirs = append(dirs, entry.Name())
		}
	}
	sort.Strings(dirs)
	return dirs
}

func childDirsWithFiles(root string, suffix string) []string {
	var dirs []string
	for _, dir := range childDirs(root) {
		if dirHasFileSuffix(filepath.Join(root, dir), suffix) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

func dirHasFileSuffix(root string, suffix string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), suffix) {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func validateContractName(name string) error {
	if !contractNamePattern.MatchString(name) || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("invalid contract name %q", name)
	}
	return nil
}

func sanitizeContractName(value string) string {
	value = strings.Trim(value, ".-_ ")
	var out strings.Builder
	lastDot := false
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
			out.WriteRune(char)
			lastDot = false
		case char == '.' || char == '-' || char == '_':
			if !lastDot {
				out.WriteRune('.')
				lastDot = true
			}
		default:
			if !lastDot {
				out.WriteRune('.')
				lastDot = true
			}
		}
	}
	sanitized := strings.Trim(out.String(), ".")
	if sanitized == "" {
		return "project.core"
	}
	return sanitized
}

func renderIntentYAML(intent Intent) string {
	var b bytes.Buffer
	writeYAMLScalar(&b, "version", IntentSchemaVersion)
	writeYAMLScalar(&b, "feature", intent.Feature)
	writeYAMLScalar(&b, "owner", intent.Owner)
	writeYAMLList(&b, "owned_paths", intent.OwnedPaths)
	writeYAMLScalar(&b, "risk", string(intent.Risk))
	if intent.Goal != "" {
		writeYAMLScalar(&b, "goal", intent.Goal)
	}
	writeYAMLList(&b, "behavior", intent.Behavior)
	writeYAMLList(&b, "security", intent.Security)
	writeYAMLList(&b, "required_tests", intent.RequiredTests)
	writeYAMLList(&b, "runtime_metrics", intent.RuntimeMetrics)
	writeYAMLList(&b, "runtime_alerts", intent.RuntimeAlerts)
	b.WriteString("rollback:\n")
	writeYAMLIndentedScalar(&b, "strategy", intent.Rollback.Strategy)
	if intent.Rollback.Flag != "" {
		writeYAMLIndentedScalar(&b, "flag", intent.Rollback.Flag)
	}
	return b.String()
}

func writeYAMLScalar(b *bytes.Buffer, key string, value string) {
	fmt.Fprintf(b, "%s: %s\n", key, strconv.Quote(value))
}

func writeYAMLIndentedScalar(b *bytes.Buffer, key string, value string) {
	fmt.Fprintf(b, "  %s: %s\n", key, strconv.Quote(value))
}

func writeYAMLList(b *bytes.Buffer, key string, values []string) {
	if len(values) == 0 {
		b.WriteString(key + ": []\n")
		return
	}
	b.WriteString(key + ":\n")
	for _, value := range values {
		fmt.Fprintf(b, "  - %s\n", strconv.Quote(value))
	}
}

func gitChangedFiles(repo string, base string, head string) ([]string, error) {
	if base == "" {
		var err error
		base, err = defaultGitBaseRef(repo)
		if err != nil {
			return nil, err
		}
	}
	if head == "" {
		head = "HEAD"
	}
	args := []string{"-C", repo, "diff", "--name-only", "--diff-filter=ACMRTUXB", base + "..." + head}
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		args = []string{"-C", repo, "diff", "--name-only", "--diff-filter=ACMRTUXB", base, head}
		out, err = exec.Command("git", args...).Output()
		if err != nil {
			return nil, fmt.Errorf("git diff failed; pass --changed-file explicitly: %w", err)
		}
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func defaultGitBaseRef(repo string) (string, error) {
	if base := strings.TrimSpace(os.Getenv("VOUCH_BASE_REF")); base != "" {
		return base, nil
	}
	if base := strings.TrimSpace(os.Getenv("GITHUB_BASE_REF")); base != "" {
		return base, nil
	}
	if base, ok := gitOutput(repo, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); ok {
		return base, nil
	}
	if base, ok := gitOutput(repo, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); ok {
		return base, nil
	}
	return "", errors.New("cannot auto-detect git base ref; pass --base or --changed-file explicitly")
}

func gitOutput(repo string, args ...string) (string, bool) {
	gitArgs := append([]string{"-C", repo}, args...)
	out, err := exec.Command("git", gitArgs...).Output()
	if err != nil {
		return "", false
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", false
	}
	return value, true
}

func normalizeChangedFiles(files []string) []string {
	var normalized []string
	for _, file := range files {
		value, ok := normalizeRepoPath(file)
		if ok {
			normalized = append(normalized, value)
		} else {
			normalized = append(normalized, file)
		}
	}
	return uniqueStrings(normalized)
}

func validSpecMap(specs map[string]Spec) map[string]Spec {
	out := make(map[string]Spec)
	for id, spec := range specs {
		if len(ValidateSpec(spec)) == 0 {
			out[id] = spec
		}
	}
	return out
}

func specsForChangedFiles(symbols SymbolTable, changedFiles []string) []string {
	set := map[string]bool{}
	for _, changedFile := range changedFiles {
		normalized, ok := normalizeRepoPath(changedFile)
		if !ok {
			continue
		}
		for _, specID := range symbols.OwnersForFile(normalized) {
			set[specID] = true
		}
	}
	var specs []string
	for specID := range set {
		specs = append(specs, specID)
	}
	sort.Strings(specs)
	return specs
}

func maxSpecRisk(specs map[string]Spec, ids []string) Risk {
	var max Risk
	for _, id := range ids {
		spec, ok := specs[id]
		if !ok || !validRisk(spec.Risk) {
			continue
		}
		if max == "" || riskRank[spec.Risk] > riskRank[max] {
			max = spec.Risk
		}
	}
	return max
}

func runtimeMetricsForSpecs(specs map[string]Spec, ids []string) []string {
	var metrics []string
	for _, id := range ids {
		metrics = append(metrics, specs[id].Runtime.Metrics...)
	}
	return uniqueStrings(metrics)
}

func rollbackForSpecs(specs map[string]Spec, ids []string) ManifestRollback {
	if len(ids) == 1 {
		spec := specs[ids[0]]
		return ManifestRollback{Strategy: spec.Rollback.Strategy, Flag: spec.Rollback.Flag, Compensation: []string{}}
	}
	if len(ids) > 1 {
		return ManifestRollback{Strategy: "revert_change", Compensation: []string{}}
	}
	return ManifestRollback{Compensation: []string{}}
}

func canaryInitialPercent(risk Risk) int {
	if riskRank[risk] >= riskRank[RiskHigh] {
		return 5
	}
	return 0
}

func resolveRepoOutput(repo string, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(repo, path)
}

func obligationsForEvidenceKind(symbols SymbolTable, kind EvidenceKind) []string {
	var ids []string
	for id, obligation := range symbols.Obligations {
		if kind == EvidenceVerifierOutput || obligation.RequiredEvidence == kind {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func importArtifactCoverage(data []byte, kind EvidenceKind, candidateObligations []string) ([]string, []string) {
	if kind == EvidenceTestCoverage {
		covered, _, issues := importJUnitEvidence(data, candidateObligations)
		return covered, issues
	}
	if kind == EvidenceSecurityCheck && sarifLooksLike(data) {
		return importSARIFReferences(data, candidateObligations)
	}
	return importGenericEvidence(data, candidateObligations)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := []string{}
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

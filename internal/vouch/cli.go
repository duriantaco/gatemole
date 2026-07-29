package vouch

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	bootstrap "github.com/duriantaco/vouch/internal/vouch/bootstrap"
)

func Main(args []string, stdout io.Writer, stderr io.Writer) int {
	common, rest, err := parseCommonArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if len(rest) == 0 {
		usage(stderr)
		return 2
	}
	absRepo, err := filepath.Abs(common.repo)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	manifest := common.manifest
	if manifest == "" {
		manifest = DefaultManifest(absRepo)
	} else if !filepath.IsAbs(manifest) {
		if _, err := os.Stat(manifest); err != nil {
			manifest = filepath.Join(absRepo, manifest)
		}
	}
	switch rest[0] {
	case "daemon":
		return daemonCommand(absRepo, rest[1:], stdout, stderr)
	case "runtime":
		return runtimeCommand(absRepo, rest[1:], common.json, stdout, stderr)
	case "doctor":
		return runtimeDoctorCommand(absRepo, rest[1:], common.json, stdout, stderr)
	case "run":
		return runtimeRunCommand(absRepo, rest[1:], common.json, stdout, stderr)
	case "status":
		return transactionStatusCommand(absRepo, rest[1:], common.json, stdout, stderr)
	case "approve":
		return transactionApproveCommand(absRepo, rest[1:], common.json, stdout, stderr)
	case "release":
		return transactionReleaseCommand(absRepo, rest[1:], common.json, stdout, stderr)
	case "kernel":
		return kernelCommand(absRepo, rest[1:], common.json, stdout, stderr)
	case "tx":
		return transactionCommand(absRepo, rest[1:], common.json, stdout, stderr)
	case "contracts":
		return contractsCommand(absRepo, common.manifest, common.json, rest[1:], stdout, stderr)
	case "approval":
		return approvalCommand(absRepo, rest[1:], common.json, stdout, stderr)
	case "identity":
		return identityCommand(absRepo, rest[1:], common.json, stdout, stderr)
	case "action":
		return actionCommand(absRepo, rest[1:], common.json, stdout, stderr)
	case "try":
		return tryCommand(absRepo, rest[1:], common.json, stdout, stderr)
	case "init":
		return initCommand(absRepo, rest[1:], common.json, stdout, stderr)
	case "bootstrap":
		return bootstrapCommand(absRepo, rest[1:], common.json, stdout, stderr)
	case "compile":
		return compileCommand(absRepo, rest[1:], common.json, stdout, stderr)
	case "intent":
		if len(rest) >= 2 && rest[1] == "parse" {
			return intentParse(rest[2:], stdout, stderr)
		}
		if len(rest) >= 2 && rest[1] == "compile" {
			return intentCompile(rest[2:], stdout, stderr)
		}
	case "ir":
		if len(rest) >= 2 && rest[1] == "build" {
			return irBuild(rest[2:], stdout, stderr)
		}
	case "plan":
		if len(rest) >= 2 && rest[1] == "build" {
			return planBuild(rest[2:], stdout, stderr, manifest)
		}
	case "artifacts":
		if len(rest) >= 2 && rest[1] == "build" {
			return artifactsBuild(rest[2:], stdout, stderr)
		}
	case "spec":
		if len(rest) == 2 && rest[1] == "lint" {
			return specLint(absRepo, stdout, stderr)
		}
	case "contract":
		if len(rest) >= 2 && rest[1] == "suggest" {
			return contractSuggest(absRepo, rest[2:], common.json, stdout, stderr)
		}
		if len(rest) >= 2 && rest[1] == "create" {
			return contractCreate(absRepo, rest[2:], common.json, stdout, stderr)
		}
	case "manifest":
		if len(rest) == 2 && rest[1] == "check" {
			return manifestCheck(absRepo, manifest, stdout, stderr)
		}
		if len(rest) >= 2 && rest[1] == "create" {
			return manifestCreate(absRepo, rest[2:], common.json, stdout, stderr)
		}
		if len(rest) >= 2 && rest[1] == "attach-artifact" {
			return manifestAttachArtifact(absRepo, rest[2:], common.json, stdout, stderr, manifest)
		}
	case "junit":
		if len(rest) >= 2 && rest[1] == "map" {
			return junitMap(absRepo, rest[2:], common.json, stdout, stderr, manifest)
		}
	case "policy":
		if len(rest) >= 2 && rest[1] == "simulate" {
			return policySimulate(absRepo, rest[2:], common.json, stdout, stderr, manifest)
		}
	case "verify":
		return collectAndRender(absRepo, manifest, common.json, stdout, stderr, "evidence", rest[1:])
	case "gate":
		return collectAndRender(absRepo, manifest, common.json, stdout, stderr, "gate", rest[1:])
	case "evidence":
		if len(rest) >= 3 && rest[1] == "import" {
			return evidenceImport(absRepo, rest[2:], common.json, stdout, stderr)
		}
		return collectAndRender(absRepo, manifest, common.json, stdout, stderr, "evidence-no-exit", rest[1:])
	}
	usage(stderr)
	return 2
}

func bootstrapCommand(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dryRun := flags.Bool("dry-run", false, "print generated contract drafts without writing files")
	check := flags.Bool("check", false, "fail when bootstrap outputs are not up to date")
	aggressive := flags.Bool("aggressive", false, "draft more obligations from path signals")
	review := flags.Bool("review", false, "print a ranked short list of contract drafts")
	reviewLimit := flags.Int("limit", 8, "maximum drafts to show with --review")
	reviewAll := flags.Bool("all", false, "show every draft with --review")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "bootstrap: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if *dryRun && *check {
		fmt.Fprintln(stderr, "bootstrap: --dry-run and --check cannot be combined")
		return 2
	}
	if (*reviewLimit != 8 || *reviewAll) && !*review {
		fmt.Fprintln(stderr, "bootstrap: --limit and --all require --review")
		return 2
	}
	if *review && *check {
		fmt.Fprintln(stderr, "bootstrap: --review and --check cannot be combined")
		return 2
	}
	if !*dryRun && !*check && !*review {
		if _, err := InitRepo(repo, "auto", false); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	result, err := bootstrap.Run(repo, bootstrap.Options{DryRun: *dryRun || *review, Check: *check, Aggressive: *aggressive})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *review {
		reviewResult := bootstrap.BuildReview(result, bootstrap.ReviewOptions{Limit: *reviewLimit, All: *reviewAll})
		if jsonOut {
			return renderCommandJSON(reviewResult, stdout, stderr)
		}
		fmt.Fprint(stdout, bootstrap.RenderReview(reviewResult))
		return 0
	}
	if jsonOut {
		if code := renderCommandJSON(result, stdout, stderr); code != 0 {
			return code
		}
	} else {
		fmt.Fprint(stdout, bootstrap.RenderText(result))
	}
	if *check && result.NeedsWrite {
		return 1
	}
	return 0
}

func compileCommand(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("compile", flag.ContinueOnError)
	flags.SetOutput(stderr)
	emit := flags.String("emit", "", "emit one compiler stage: ast, spec, ir, or plan")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "compile: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if *emit != "" && *emit != "ast" && *emit != "spec" && *emit != "ir" && *emit != "plan" {
		fmt.Fprintln(stderr, "compile: --emit must be one of ast, spec, ir, or plan")
		return 2
	}
	output, err := CompileRepo(repo)
	if err != nil {
		if diagnosticErr, ok := err.(DiagnosticError); ok {
			fmt.Fprintln(stderr, "Compile failed:")
			for _, diagnostic := range diagnosticErr.Diagnostics {
				fmt.Fprintf(stderr, "- %s\n", FormatDiagnostic(diagnostic))
			}
			return 1
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *emit != "" {
		return renderCommandJSON(output.EmitArtifact(*emit), stdout, stderr)
	}
	if jsonOut {
		return renderCommandJSON(output.Result, stdout, stderr)
	}
	fmt.Fprint(stdout, RenderCompileResult(output.Result))
	return 0
}

func initCommand(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	profile := flags.String("profile", "auto", "profile to use: auto, python, node, go, rust, generic")
	force := flags.Bool("force", false, "overwrite .vouch/config.json")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	result, err := InitRepo(repo, *profile, *force)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(result, stdout, stderr)
	}
	if result.Created {
		fmt.Fprintf(stdout, "Initialized Vouch in %s\n", result.Repo)
	} else {
		fmt.Fprintf(stdout, "Vouch already initialized in %s\n", result.Repo)
	}
	fmt.Fprintf(stdout, "Profiles: %s\n", strings.Join(result.Profiles, ", "))
	fmt.Fprintf(stdout, "Config: %s\n", result.ConfigPath)
	return 0
}

func contractSuggest(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("contract suggest", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	suggestions, err := ContractSuggestions(repo)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(suggestions, stdout, stderr)
	}
	if len(suggestions) == 0 {
		fmt.Fprintln(stdout, "No contract suggestions found.")
		return 0
	}
	for _, suggestion := range suggestions {
		fmt.Fprintf(stdout, "%s (%s, %s)\n", suggestion.Name, suggestion.Profile, suggestion.Confidence)
		for _, ownedPath := range suggestion.OwnedPaths {
			fmt.Fprintf(stdout, "- %s\n", ownedPath)
		}
	}
	return 0
}

func contractCreate(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("contract create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("name", "", "contract name")
	owner := flags.String("owner", "", "contract owner")
	risk := flags.String("risk", "", "risk: low, medium, high, critical")
	goal := flags.String("goal", "", "contract goal")
	rollbackStrategy := flags.String("rollback-strategy", "", "rollback strategy")
	rollbackFlag := flags.String("rollback-flag", "", "rollback flag")
	force := flags.Bool("force", false, "overwrite existing intent/spec")
	var paths stringListFlag
	var behavior stringListFlag
	var security stringListFlag
	var requiredTests stringListFlag
	var metrics stringListFlag
	var alerts stringListFlag
	flags.Var(&paths, "paths", "owned path pattern, repeatable or comma-separated")
	flags.Var(&behavior, "behavior", "behavior obligation, repeatable")
	flags.Var(&security, "security", "security invariant, repeatable")
	flags.Var(&requiredTests, "required-test", "required test obligation, repeatable")
	flags.Var(&metrics, "metric", "runtime metric, repeatable")
	flags.Var(&alerts, "alert", "runtime alert, repeatable")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *name == "" || *owner == "" || *risk == "" || len(paths) == 0 || len(behavior) == 0 || len(requiredTests) == 0 {
		fmt.Fprintln(stderr, "contract create requires --name, --owner, --risk, --paths, --behavior, and --required-test")
		return 2
	}
	intent := Intent{
		Feature:        *name,
		Owner:          *owner,
		OwnedPaths:     []string(paths),
		Risk:           Risk(*risk),
		Goal:           *goal,
		Behavior:       []string(behavior),
		Security:       []string(security),
		RequiredTests:  []string(requiredTests),
		RuntimeMetrics: []string(metrics),
		RuntimeAlerts:  []string(alerts),
		Rollback: SpecRollback{
			Strategy: *rollbackStrategy,
			Flag:     *rollbackFlag,
		},
	}
	spec, intentPath, specPath, err := CreateContract(repo, intent, *force)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(map[string]any{
			"intent_path": intentPath,
			"spec_path":   specPath,
			"spec":        spec,
		}, stdout, stderr)
	}
	fmt.Fprintf(stdout, "Created contract %s\n", spec.ID)
	fmt.Fprintf(stdout, "Intent: %s\n", intentPath)
	fmt.Fprintf(stdout, "Spec: %s\n", specPath)
	return 0
}

func manifestCreate(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("manifest create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	taskID := flags.String("task-id", "", "task id")
	summary := flags.String("summary", "", "task summary")
	agent := flags.String("agent", "", "agent name")
	runID := flags.String("run-id", "", "agent run id")
	model := flags.String("model", "", "agent model")
	runnerIdentity := flags.String("runner-identity", "", "expected runner identity for signed evidence")
	runnerOIDCIssuer := flags.String("runner-oidc-issuer", "", "expected runner OIDC issuer for signed evidence")
	base := flags.String("base", "", "git base ref; auto-detected when omitted")
	head := flags.String("head", "HEAD", "git head ref")
	risk := flags.String("risk", "", "risk override")
	outPath := flags.String("out", "", "manifest output path")
	migrationChanged := flags.Bool("migration-changed", false, "whether migrations changed")
	var changedFiles stringListFlag
	var externalEffects stringListFlag
	flags.Var(&changedFiles, "changed-file", "changed file, repeatable")
	flags.Var(&externalEffects, "external-effect", "external side effect, repeatable")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	manifest, err := CreateManifest(repo, ManifestCreateOptions{
		TaskID:           *taskID,
		Summary:          *summary,
		Agent:            *agent,
		RunID:            *runID,
		Model:            *model,
		RunnerIdentity:   *runnerIdentity,
		RunnerOIDCIssuer: *runnerOIDCIssuer,
		Base:             *base,
		Head:             *head,
		Risk:             Risk(*risk),
		ChangedFiles:     []string(changedFiles),
		ExternalEffects:  []string(externalEffects),
		MigrationChanged: *migrationChanged,
		Out:              *outPath,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(manifest, stdout, stderr)
	}
	fmt.Fprintf(stdout, "Created manifest %s\n", resolveRepoOutput(repo, *outPath))
	if len(manifest.Change.SpecsTouched) == 0 {
		fmt.Fprintln(stdout, "No specs matched changed files; manifest check will require owned_paths coverage.")
	} else {
		fmt.Fprintf(stdout, "Specs touched: %s\n", strings.Join(manifest.Change.SpecsTouched, ", "))
	}
	return 0
}

func manifestAttachArtifact(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer, defaultManifest string) int {
	flags := flag.NewFlagSet("manifest attach-artifact", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "manifest path")
	id := flags.String("id", "", "artifact id")
	kind := flags.String("kind", "", "artifact kind")
	path := flags.String("path", "", "artifact path")
	testMap := flags.String("test-map", "", "test map path for raw JUnit test_coverage artifacts")
	producer := flags.String("producer", "", "artifact producer")
	command := flags.String("command", "", "command that produced artifact")
	sha256 := flags.String("sha256", "", "expected sha256")
	evidenceBundle := flags.String("evidence-bundle", "", "Vouch evidence bundle path")
	signatureBundle := flags.String("signature-bundle", "", "cosign signature bundle path")
	signerIdentity := flags.String("signer-identity", "", "expected cosign signer identity")
	signerOIDCIssuer := flags.String("signer-oidc-issuer", "", "expected cosign signer OIDC issuer")
	outPath := flags.String("out", "", "updated manifest output path")
	exitCode := flags.Int("exit-code", -1, "artifact command exit code")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "manifest attach-artifact: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if *manifestPath == "" {
		*manifestPath = defaultManifest
	}
	if *exitCode < 0 {
		fmt.Fprintln(stderr, "manifest attach-artifact requires --exit-code")
		return 2
	}
	manifest, artifact, err := AttachArtifact(repo, AttachArtifactOptions{
		ManifestPath:     *manifestPath,
		ID:               *id,
		Kind:             EvidenceKind(*kind),
		Path:             *path,
		TestMapPath:      *testMap,
		Producer:         *producer,
		Command:          *command,
		ExitCode:         *exitCode,
		SHA256:           *sha256,
		EvidenceBundle:   *evidenceBundle,
		SignatureBundle:  *signatureBundle,
		SignerIdentity:   *signerIdentity,
		SignerOIDCIssuer: *signerOIDCIssuer,
		Out:              *outPath,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(map[string]any{
			"manifest": manifest,
			"artifact": artifact,
		}, stdout, stderr)
	}
	fmt.Fprintf(stdout, "Attached artifact %s (%s)\n", artifact.ID, artifact.Kind)
	fmt.Fprintf(stdout, "Covered obligations: %d\n", len(artifact.Obligations))
	return 0
}

func junitMap(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer, defaultManifest string) int {
	flags := flag.NewFlagSet("junit map", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "manifest path")
	junitPath := flags.String("junit", "", "raw JUnit XML path")
	testMapPath := flags.String("test-map", "", "test map path")
	outPath := flags.String("out", "", "mapped JUnit XML output path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "junit map: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if *manifestPath == "" {
		*manifestPath = defaultManifest
	}
	result, err := MapJUnitEvidence(repo, JUnitMapOptions{
		ManifestPath: *manifestPath,
		JUnitPath:    *junitPath,
		TestMapPath:  *testMapPath,
		Out:          *outPath,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(result, stdout, stderr)
	}
	fmt.Fprintf(stdout, "Mapped JUnit %s -> %s\n", result.InputPath, result.OutputPath)
	fmt.Fprintf(stdout, "Covered obligations: %d\n", len(result.CoveredObligations))
	return 0
}

func evidenceImport(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "evidence import requires a format: junit")
		return 2
	}
	format := args[0]
	flags := flag.NewFlagSet("evidence import "+format, flag.ContinueOnError)
	flags.SetOutput(stderr)
	outPath := flags.String("out", "", "evidence manifest output path")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if format != "junit" {
		fmt.Fprintf(stderr, "evidence import: unsupported format %q\n", format)
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "evidence import junit requires exactly one JUnit XML path")
		return 2
	}
	result, err := ImportJUnitEvidence(repo, EvidenceImportOptions{
		ArtifactPath: flags.Arg(0),
		Out:          *outPath,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(result, stdout, stderr)
	}
	fmt.Fprintf(stdout, "Imported JUnit evidence %s -> %s\n", result.InputPath, result.OutputPath)
	fmt.Fprintf(stdout, "Linked obligations: %d\n", len(result.Links))
	if len(result.UnmatchedObligations) > 0 {
		fmt.Fprintf(stdout, "Unmatched obligations: %d\n", len(result.UnmatchedObligations))
	}
	return 0
}

func renderCommandJSON(value any, stdout io.Writer, stderr io.Writer) int {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, string(data))
	return 0
}

type commonArgs struct {
	repo     string
	manifest string
	json     bool
}

func parseCommonArgs(args []string) (commonArgs, []string, error) {
	common := commonArgs{repo: "."}
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			rest = append(rest, args[i:]...)
			return common, rest, nil
		case arg == "--json":
			common.json = true
		case arg == "--repo" || arg == "--manifest":
			if i+1 >= len(args) {
				return common, nil, fmt.Errorf("%s requires a value", arg)
			}
			if arg == "--repo" {
				common.repo = args[i+1]
			} else {
				common.manifest = args[i+1]
			}
			i++
		case strings.HasPrefix(arg, "--repo="):
			common.repo = strings.TrimPrefix(arg, "--repo=")
		case strings.HasPrefix(arg, "--manifest="):
			common.manifest = strings.TrimPrefix(arg, "--manifest=")
		case strings.HasPrefix(arg, "--json="):
			value := strings.TrimPrefix(arg, "--json=")
			if value != "true" && value != "false" {
				return common, nil, errors.New("--json must be true or false")
			}
			common.json = value == "true"
		default:
			rest = append(rest, arg)
		}
	}
	return common, rest, nil
}

func intentParse(args []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("intent parse", flag.ContinueOnError)
	flags.SetOutput(stderr)
	intentPath := flags.String("intent", "", "path to YAML intent file")
	outPath := flags.String("out", "", "path to write AST JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *intentPath == "" || *outPath == "" {
		fmt.Fprintln(stderr, "intent parse requires --intent and --out")
		return 2
	}
	ast, err := WriteIntentASTFile(*intentPath, *outPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "Parsed intent %s -> AST %s (%d node(s))\n", *intentPath, *outPath, len(ast.Nodes))
	return 0
}

func intentCompile(args []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("intent compile", flag.ContinueOnError)
	flags.SetOutput(stderr)
	intentPath := flags.String("intent", "", "path to YAML intent file")
	outPath := flags.String("out", "", "path to write compiled spec JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *intentPath == "" || *outPath == "" {
		fmt.Fprintln(stderr, "intent compile requires --intent and --out")
		return 2
	}
	spec, err := CompileIntentFile(*intentPath, *outPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "Compiled intent %s -> spec %s (%s)\n", *intentPath, *outPath, spec.ID)
	return 0
}

func irBuild(args []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("ir build", flag.ContinueOnError)
	flags.SetOutput(stderr)
	specPath := flags.String("spec", "", "path to spec JSON file")
	outPath := flags.String("out", "", "path to write IR JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *specPath == "" || *outPath == "" {
		fmt.Fprintln(stderr, "ir build requires --spec and --out")
		return 2
	}
	ir, err := BuildIRFile(*specPath, *outPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "Built IR %s -> %s (%s)\n", *specPath, *outPath, ir.Feature)
	return 0
}

func planBuild(args []string, stdout io.Writer, stderr io.Writer, defaultManifest string) int {
	flags := flag.NewFlagSet("plan build", flag.ContinueOnError)
	flags.SetOutput(stderr)
	specPath := flags.String("spec", "", "path to spec JSON file")
	manifestPath := flags.String("manifest", "", "path to change manifest JSON")
	outPath := flags.String("out", "", "path to write verification plan JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *manifestPath == "" {
		*manifestPath = defaultManifest
	}
	if *specPath == "" || *manifestPath == "" || *outPath == "" {
		fmt.Fprintln(stderr, "plan build requires --spec, --manifest, and --out")
		return 2
	}
	plan, err := BuildVerificationPlanFile(*specPath, *manifestPath, *outPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "Built verification plan %s + %s -> %s (%s)\n", *specPath, *manifestPath, *outPath, plan.Feature)
	return 0
}

func artifactsBuild(args []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("artifacts build", flag.ContinueOnError)
	flags.SetOutput(stderr)
	specPath := flags.String("spec", "", "path to spec JSON file")
	outDir := flags.String("out", "", "directory to write generated artifacts")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *specPath == "" || *outDir == "" {
		fmt.Fprintln(stderr, "artifacts build requires --spec and --out")
		return 2
	}
	if err := BuildArtifacts(*specPath, *outDir); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "Built artifacts from %s -> %s\n", *specPath, *outDir)
	return 0
}

func specLint(repo string, stdout io.Writer, stderr io.Writer) int {
	specs, err := LoadSpecs(repo)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(specs) == 0 {
		fmt.Fprintln(stdout, "No specs found.")
		return 1
	}
	var errors []string
	for _, spec := range specs {
		errors = append(errors, ValidateSpec(spec)...)
	}
	if len(errors) > 0 {
		fmt.Fprintln(stdout, "Spec lint failed:")
		for _, err := range errors {
			fmt.Fprintf(stdout, "- %s\n", err)
		}
		return 1
	}
	fmt.Fprintf(stdout, "Spec lint passed: %d spec(s)\n", len(specs))
	return 0
}

func manifestCheck(repo string, manifestPath string, stdout io.Writer, stderr io.Writer) int {
	specs, err := LoadSpecs(repo)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	manifest, err := LoadJSON[Manifest](manifestPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	errors := CompileManifestPipeline(specs, manifest).ManifestErrors
	if len(errors) > 0 {
		fmt.Fprintln(stdout, "Manifest check failed:")
		for _, err := range errors {
			fmt.Fprintf(stdout, "- %s\n", err)
		}
		return 1
	}
	fmt.Fprintln(stdout, "Manifest check passed")
	return 0
}

func policySimulate(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer, defaultManifest string) int {
	flags := flag.NewFlagSet("policy simulate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "manifest path")
	policyPath := flags.String("policy", "", "release policy path")
	requireSigned := flags.Bool("require-signed", false, "require cosign-verified evidence artifacts")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "policy simulate: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if *manifestPath == "" {
		*manifestPath = defaultManifest
	}
	evidence, err := CollectEvidenceWithOptions(repo, *manifestPath, CollectEvidenceOptions{
		RequireSigned: *requireSigned,
		PolicyPath:    *policyPath,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	simulation := PolicySimulation{
		Version:    PolicySimulationVersion,
		PolicyPath: evidence.PolicyPath,
		Input:      PolicyInputFromEvidence(evidence),
		Result:     evidence.PolicyResult,
	}
	if jsonOut {
		return renderCommandJSON(simulation, stdout, stderr)
	}
	fmt.Fprintf(stdout, "Policy: %s\n", simulation.PolicyPath)
	fmt.Fprintf(stdout, "Decision: %s\n", simulation.Result.Decision)
	if len(simulation.Result.RulesFired) > 0 {
		fmt.Fprintf(stdout, "Rules fired: %s\n", strings.Join(simulation.Result.RulesFired, ", "))
	}
	for _, reason := range simulation.Result.Reasons {
		fmt.Fprintf(stdout, "- %s\n", reason)
	}
	return 0
}

func collectAndRender(repo string, manifestPath string, jsonOut bool, stdout io.Writer, stderr io.Writer, mode string, args []string) int {
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.SetOutput(stderr)
	gateOut := ""
	requireSigned := false
	githubSummary := false
	verbose := false
	explain := false
	policyPath := flags.String("policy", "", "release policy path")
	if mode == "gate" {
		flags.StringVar(&gateOut, "out", "", "path to write compact gate result JSON")
		flags.BoolVar(&requireSigned, "require-signed", false, "require cosign-verified evidence artifacts")
		flags.BoolVar(&githubSummary, "github-summary", false, "append a Markdown gate summary to GITHUB_STEP_SUMMARY")
		flags.BoolVar(&verbose, "verbose", false, "print detailed component-level gate output")
		flags.BoolVar(&explain, "explain", false, "include evidence type explanations in human gate output")
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", mode, flags.Arg(0))
		return 2
	}
	var evidence Evidence
	var err error
	if shouldUseEvidenceManifestFallback(repo, manifestPath) {
		evidence, err = CollectEvidenceFromEvidenceManifest(repo, DefaultEvidenceManifest(repo), CollectEvidenceOptions{RequireSigned: requireSigned, PolicyPath: *policyPath})
	} else {
		evidence, err = CollectEvidenceWithOptions(repo, manifestPath, CollectEvidenceOptions{RequireSigned: requireSigned, PolicyPath: *policyPath})
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if gateOut != "" {
		if err := writeJSONFile(resolveRepoOutput(repo, gateOut), GateResultFromEvidence(evidence)); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if githubSummary {
		if err := appendGitHubSummary(evidence); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if jsonOut {
		var output string
		var err error
		if mode == "gate" {
			output, err = RenderGateResultJSON(evidence)
		} else {
			output, err = RenderJSON(evidence)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprint(stdout, output)
	} else {
		switch mode {
		case "gate":
			if verbose {
				fmt.Fprint(stdout, RenderGateVerbose(evidence))
			} else {
				fmt.Fprint(stdout, RenderGateWithOptions(evidence, GateRenderOptions{Explain: explain}))
			}
		default:
			fmt.Fprint(stdout, RenderEvidence(evidence))
		}
	}
	if mode == "evidence-no-exit" {
		return 0
	}
	if evidence.Decision == "block" {
		return 1
	}
	return 0
}

func usage(out io.Writer) {
	fmt.Fprintln(out, "usage: vouch [--repo DIR] [--manifest FILE] [--json] <command>")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "commands:")
	fmt.Fprintln(out, "  daemon [--db FILE] [--socket FILE]")
	fmt.Fprintln(out, "  runtime init --agent NAME --image IMAGE@sha256:DIGEST --source-digest sha256:DIGEST -- COMMAND [ARG...]")
	fmt.Fprintln(out, "  doctor [--agent NAME] [--runtime-engine ENGINE] [--agent-profiles FILE] [--socket FILE]")
	fmt.Fprintln(out, "  run (--intent TEXT | --intent-file FILE) (--agent NAME [-- AGENT_ARG...] | --image IMAGE -- COMMAND [ARG...])")
	fmt.Fprintln(out, "  status ID [--namespace NS]")
	fmt.Fprintln(out, "  approve ID [--namespace NS] --key FILE --key-id ID --approver ID --class CLASS")
	fmt.Fprintln(out, "  release ID [--namespace NS]")
	fmt.Fprintln(out, "  tx create|start|worktree|stage|validate|verify|prepare|get|list|effects|events|abort (advanced transaction lifecycle)")
	fmt.Fprintln(out, "  kernel run create|get|list|events|transition|pause|resume|cancel|grant (low-level run lifecycle)")
	fmt.Fprintln(out, "  contracts <command> (optional release-contract verification module)")
	fmt.Fprintln(out, "  approval keygen --key-id ID --principal ID --class CLASS --private-key FILE --trust-file FILE")
	fmt.Fprintln(out, "  identity keygen|issue (local OIDC bootstrap and acceptance testing)")
	fmt.Fprintln(out, "  action fs-write|fs-read (use 'vouch action' for details)")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Legacy top-level Contracts commands remain accepted for compatibility.")
}

func appendGitHubSummary(evidence Evidence) error {
	path := strings.TrimSpace(os.Getenv("GITHUB_STEP_SUMMARY"))
	if path == "" {
		return errors.New("gate --github-summary requires GITHUB_STEP_SUMMARY to be set")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := fmt.Fprint(file, RenderGitHubSummary(evidence)); err != nil {
		return err
	}
	if _, err := fmt.Fprint(file, "\n"); err != nil {
		return err
	}
	return nil
}

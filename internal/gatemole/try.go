package gatemole

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	bootstrap "github.com/duriantaco/gatemole/internal/gatemole/bootstrap"
)

const tryResultVersion = "gatemole.try.v0"

type TryResult struct {
	Version              string            `json:"version"`
	Repo                 string            `json:"repo"`
	Mode                 string            `json:"mode"`
	Snapshot             string            `json:"snapshot,omitempty"`
	SnapshotKept         bool              `json:"snapshot_kept,omitempty"`
	GitBranch            string            `json:"git_branch,omitempty"`
	DirtyEntries         int               `json:"dirty_entries,omitempty"`
	FilesCopied          int               `json:"files_copied,omitempty"`
	Drafts               int               `json:"drafts"`
	CompiledSpecs        int               `json:"compiled_specs"`
	CompiledObligations  int               `json:"compiled_obligations"`
	TestsDiscovered      int               `json:"tests_discovered"`
	RiskCounts           map[string]int    `json:"risk_counts"`
	TopDrafts            []TryDraftSummary `json:"top_drafts"`
	JUnitLinks           int               `json:"junit_links,omitempty"`
	GateDecision         string            `json:"gate_decision,omitempty"`
	CoveredObligations   int               `json:"covered_obligations,omitempty"`
	RequiredObligations  int               `json:"required_obligations,omitempty"`
	TestCommand          string            `json:"test_command,omitempty"`
	TestCommandExitCode  *int              `json:"test_command_exit_code,omitempty"`
	Warnings             []string          `json:"warnings,omitempty"`
	RecommendedNextSteps []string          `json:"recommended_next_steps"`
}

type TryDraftSummary struct {
	Component   string   `json:"component"`
	Risk        string   `json:"risk"`
	Why         string   `json:"why"`
	Tests       int      `json:"tests"`
	Obligations int      `json:"obligations"`
	Edit        string   `json:"edit"`
	Paths       []string `json:"paths,omitempty"`
}

type tryOptions struct {
	JUnit       string
	TestCommand string
	Write       bool
	Keep        bool
}

type snapshotInfo struct {
	Path         string
	GitBranch    string
	DirtyEntries int
	FilesCopied  int
}

func tryCommand(repo string, args []string, jsonOut bool, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("try", flag.ContinueOnError)
	flags.SetOutput(stderr)
	opts := tryOptions{}
	flags.StringVar(&opts.JUnit, "junit", "", "JUnit XML path to import after compile")
	flags.StringVar(&opts.TestCommand, "test-command", "", "test command to run before importing JUnit")
	flags.BoolVar(&opts.Write, "write", false, "write generated Vouch files into the source repo")
	flags.BoolVar(&opts.Keep, "keep", false, "keep the temporary snapshot after the run")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "try: unexpected argument %q\n", flags.Arg(0))
		return 2
	}

	result, err := runTry(repo, opts)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(result, stdout, stderr)
	}
	fmt.Fprint(stdout, RenderTryResult(result))
	return 0
}

func runTry(repo string, opts tryOptions) (TryResult, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return TryResult{}, err
	}
	target := absRepo
	result := TryResult{
		Version:              tryResultVersion,
		Repo:                 absRepo,
		Mode:                 "write",
		RiskCounts:           map[string]int{},
		RecommendedNextSteps: []string{},
	}
	cleanup := func() {}
	if !opts.Write {
		info, err := snapshotRepo(absRepo)
		if err != nil {
			return TryResult{}, err
		}
		target = info.Path
		result.Mode = "snapshot"
		result.Snapshot = info.Path
		result.SnapshotKept = opts.Keep
		result.GitBranch = info.GitBranch
		result.DirtyEntries = info.DirtyEntries
		result.FilesCopied = info.FilesCopied
		if !opts.Keep {
			cleanup = func() {
				_ = os.RemoveAll(info.Path)
			}
		}
		if opts.JUnit != "" {
			if _, err := copySnapshotInputFile(absRepo, target, opts.JUnit); err != nil {
				return TryResult{}, err
			}
		}
	}
	defer cleanup()

	if _, err := InitRepo(target, "auto", false); err != nil {
		return TryResult{}, err
	}
	dryRun, err := bootstrap.Run(target, bootstrap.Options{DryRun: true})
	if err != nil {
		return TryResult{}, err
	}
	if _, err := bootstrap.Run(target, bootstrap.Options{}); err != nil {
		return TryResult{}, err
	}
	compileOutput, err := CompileRepo(target)
	if err != nil {
		return TryResult{}, err
	}

	result.Drafts = len(dryRun.Drafts)
	result.TestsDiscovered = countBootstrapTests(dryRun.Drafts)
	result.RiskCounts = countDraftRisks(dryRun.Drafts)
	result.TopDrafts = summarizeTryDrafts(dryRun.Drafts, 5)
	result.CompiledSpecs = compileOutput.Result.SpecsCompiled
	result.CompiledObligations = compileOutput.Result.ObligationsBuilt
	if opts.TestCommand != "" {
		exitCode, warning := runTryTestCommand(target, opts.TestCommand)
		result.TestCommand = opts.TestCommand
		result.TestCommandExitCode = &exitCode
		if warning != "" {
			result.Warnings = append(result.Warnings, warning)
		}
	}

	if opts.JUnit != "" {
		importResult, err := ImportJUnitEvidence(target, EvidenceImportOptions{ArtifactPath: opts.JUnit})
		if err != nil {
			return TryResult{}, err
		}
		result.JUnitLinks = len(importResult.Links)
		evidence, err := CollectEvidenceFromEvidenceManifest(target, DefaultEvidenceManifest(target), CollectEvidenceOptions{})
		if err != nil {
			return TryResult{}, err
		}
		result.GateDecision = evidence.Decision
		result.CoveredObligations, result.RequiredObligations = obligationCoverageCounts(evidence)
	}

	result.RecommendedNextSteps = tryNextSteps(result, opts)
	return result, nil
}

func snapshotRepo(repo string) (snapshotInfo, error) {
	tmp, err := os.MkdirTemp("", "gatemole-try-*")
	if err != nil {
		return snapshotInfo{}, err
	}
	info := snapshotInfo{Path: tmp}
	if isGitWorktree(repo) {
		files, err := gitTrackedAndUnignoredFiles(repo)
		if err != nil {
			_ = os.RemoveAll(tmp)
			return snapshotInfo{}, err
		}
		for _, rel := range files {
			copied, err := copyRepoFile(repo, tmp, rel)
			if err != nil {
				_ = os.RemoveAll(tmp)
				return snapshotInfo{}, err
			}
			if copied {
				info.FilesCopied++
			}
		}
		info.GitBranch = gitBranch(repo)
		info.DirtyEntries = gitDirtyEntries(repo)
		return info, nil
	}
	copied, err := copyFilesystemSnapshot(repo, tmp)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return snapshotInfo{}, err
	}
	info.FilesCopied = copied
	return info, nil
}

func isGitWorktree(repo string) bool {
	cmd := exec.Command("git", "-C", repo, "rev-parse", "--is-inside-work-tree")
	return cmd.Run() == nil
}

func gitTrackedAndUnignoredFiles(repo string) ([]string, error) {
	cmd := exec.Command("git", "-C", repo, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(out, []byte{0})
	files := make([]string, 0, len(parts))
	for _, part := range parts {
		rel := filepath.ToSlash(string(part))
		if rel == "" || shouldSkipSnapshotRel(rel) {
			continue
		}
		files = append(files, rel)
	}
	sort.Strings(files)
	return files, nil
}

func gitBranch(repo string) string {
	cmd := exec.Command("git", "-C", repo, "branch", "--show-current")
	out, err := cmd.Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return strings.TrimSpace(string(out))
	}
	cmd = exec.Command("git", "-C", repo, "rev-parse", "--short", "HEAD")
	out, err = cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitDirtyEntries(repo string) int {
	cmd := exec.Command("git", "-C", repo, "status", "--porcelain", "--untracked-files=all")
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func copyFilesystemSnapshot(src string, dst string) (int, error) {
	copied := 0
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == src {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if shouldSkipSnapshotRel(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		ok, err := copyRepoFile(src, dst, rel)
		if err != nil {
			return err
		}
		if ok {
			copied++
		}
		return nil
	})
	return copied, err
}

func copyRepoFile(srcRoot string, dstRoot string, rel string) (bool, error) {
	rel = filepath.Clean(filepath.FromSlash(rel))
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false, nil
	}
	src := filepath.Join(srcRoot, rel)
	info, err := os.Stat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if info.IsDir() {
		return false, nil
	}
	dst := filepath.Join(dstRoot, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return false, err
	}
	if err := out.Close(); err != nil {
		return false, err
	}
	return true, nil
}

func copySnapshotInputFile(srcRoot string, dstRoot string, rel string) (bool, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return false, nil
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false, nil
	}
	return copyRepoFile(srcRoot, dstRoot, filepath.ToSlash(clean))
}

func shouldSkipSnapshotRel(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 0 {
		return false
	}
	switch parts[0] {
	case ".git", "node_modules", ".venv", "venv", "target", "dist", "build", "__pycache__":
		return true
	default:
		return false
	}
}

func runTryTestCommand(repo string, command string) (int, string) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = repo
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if err == nil {
		return 0, ""
	}
	exitCode := 1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	tail := strings.TrimSpace(output.String())
	if len(tail) > 500 {
		tail = tail[len(tail)-500:]
	}
	if tail == "" {
		return exitCode, fmt.Sprintf("test command exited with %d", exitCode)
	}
	return exitCode, fmt.Sprintf("test command exited with %d: %s", exitCode, tail)
}

func countBootstrapTests(drafts []bootstrap.Draft) int {
	seen := map[string]bool{}
	for _, draft := range drafts {
		for _, signal := range draft.Signals {
			if signal.Type != "test" {
				continue
			}
			key := signal.File + "::" + signal.Symbol
			if key != "::" {
				seen[key] = true
			}
		}
	}
	return len(seen)
}

func countDraftRisks(drafts []bootstrap.Draft) map[string]int {
	counts := map[string]int{}
	for _, draft := range drafts {
		counts[draft.Risk]++
	}
	return counts
}

func summarizeTryDrafts(drafts []bootstrap.Draft, limit int) []TryDraftSummary {
	ranked := append([]bootstrap.Draft(nil), drafts...)
	sort.Slice(ranked, func(i, j int) bool {
		leftRisk := riskRank[Risk(ranked[i].Risk)]
		rightRisk := riskRank[Risk(ranked[j].Risk)]
		if leftRisk != rightRisk {
			return leftRisk > rightRisk
		}
		if len(ranked[i].Obligations) != len(ranked[j].Obligations) {
			return len(ranked[i].Obligations) > len(ranked[j].Obligations)
		}
		return ranked[i].Component < ranked[j].Component
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]TryDraftSummary, 0, len(ranked))
	for _, draft := range ranked {
		out = append(out, TryDraftSummary{
			Component:   draft.Component,
			Risk:        draft.Risk,
			Why:         tryDraftWhy(draft),
			Tests:       draftTestCount(draft),
			Obligations: len(draft.Obligations),
			Edit:        draft.IntentPath,
			Paths:       firstStrings(draft.Paths, 3),
		})
	}
	return out
}

func tryDraftWhy(draft bootstrap.Draft) string {
	for _, signal := range draft.Signals {
		if signal.Type == "path" && signal.Risk != "" {
			return signal.File + " risk is " + signal.Risk
		}
	}
	for _, signal := range draft.Signals {
		if signal.Type == "test" && signal.File != "" {
			return "test coverage found in " + signal.File
		}
	}
	if len(draft.Paths) > 0 {
		return "owned paths found"
	}
	return "repository signals found"
}

func draftTestCount(draft bootstrap.Draft) int {
	count := 0
	for _, signal := range draft.Signals {
		if signal.Type == "test" {
			count++
		}
	}
	return count
}

func firstStrings(values []string, limit int) []string {
	if len(values) <= limit {
		return append([]string(nil), values...)
	}
	return append([]string(nil), values[:limit]...)
}

func tryNextSteps(result TryResult, opts tryOptions) []string {
	var steps []string
	if len(result.TopDrafts) > 0 {
		steps = append(steps, "review "+result.TopDrafts[0].Edit)
	}
	if !opts.Write {
		steps = append(steps, "write drafts with: vouch try --repo "+shellQuotePath(result.Repo)+" --write")
	}
	if opts.JUnit == "" {
		steps = append(steps, "link tests with: vouch try --repo "+shellQuotePath(result.Repo)+" --test-command \"pytest --junitxml .gatemole/artifacts/pytest.xml\" --junit .gatemole/artifacts/pytest.xml")
	} else if result.GateDecision == "block" {
		steps = append(steps, "add or attach missing security/runtime/rollback evidence, then rerun vouch gate")
	}
	return steps
}

func shellQuotePath(path string) string {
	if path == "" || strings.ContainsAny(path, " \t\n\"'\\$") {
		return fmt.Sprintf("%q", path)
	}
	return path
}

func RenderTryResult(result TryResult) string {
	var b strings.Builder
	b.WriteString("Vouch Try\n\n")
	fmt.Fprintf(&b, "Repo: %s\n", result.Repo)
	if result.Mode == "write" {
		b.WriteString("Mode: write to source repo\n")
	} else {
		b.WriteString("Mode: temp snapshot\n")
		if result.SnapshotKept && result.Snapshot != "" {
			fmt.Fprintf(&b, "Snapshot: %s\n", result.Snapshot)
		}
		if result.GitBranch != "" {
			fmt.Fprintf(&b, "Git branch: %s", result.GitBranch)
			if result.DirtyEntries > 0 {
				fmt.Fprintf(&b, " (%d dirty entries copied)", result.DirtyEntries)
			}
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')

	b.WriteString("Found:\n")
	fmt.Fprintf(&b, "  contracts drafted: %d\n", result.Drafts)
	fmt.Fprintf(&b, "  obligations compiled: %d\n", result.CompiledObligations)
	fmt.Fprintf(&b, "  high-risk drafts: %d\n", result.RiskCounts[string(RiskHigh)])
	fmt.Fprintf(&b, "  tests discovered: %d\n", result.TestsDiscovered)
	if result.Mode == "write" {
		b.WriteString("  wrote: .gatemole/\n")
	}

	if len(result.TopDrafts) > 0 {
		b.WriteString("\nMost important drafts to review:\n")
		for _, draft := range result.TopDrafts {
			fmt.Fprintf(&b, "  %s %s\n", tryRiskLabel(draft.Risk), draft.Component)
			fmt.Fprintf(&b, "    why: %s\n", draft.Why)
			fmt.Fprintf(&b, "    tests: %d mapped\n", draft.Tests)
			fmt.Fprintf(&b, "    obligations: %d\n", draft.Obligations)
			fmt.Fprintf(&b, "    edit: %s\n", draft.Edit)
		}
	}

	if result.TestCommand != "" {
		b.WriteString("\nTest command:\n")
		exitCode := 0
		if result.TestCommandExitCode != nil {
			exitCode = *result.TestCommandExitCode
		}
		fmt.Fprintf(&b, "  exit code: %d\n", exitCode)
	}
	if result.JUnitLinks > 0 || result.GateDecision != "" {
		b.WriteString("\nEvidence:\n")
		fmt.Fprintf(&b, "  JUnit linked: %d required-test obligations\n", result.JUnitLinks)
		if result.GateDecision != "" {
			b.WriteString("\nGate:\n")
			fmt.Fprintf(&b, "  decision: %s\n", result.GateDecision)
			fmt.Fprintf(&b, "  covered: %d/%d obligations\n", result.CoveredObligations, result.RequiredObligations)
			if result.GateDecision == "block" {
				b.WriteString("\nWhy blocked:\n")
				b.WriteString("  tests cover required-test obligations only\n")
				b.WriteString("  missing behavior/security/runtime/rollback evidence may still be required\n")
			}
		}
	}

	if len(result.Warnings) > 0 {
		b.WriteString("\nWarnings:\n")
		for _, warning := range result.Warnings {
			fmt.Fprintf(&b, "  %s\n", warning)
		}
	}
	if len(result.RecommendedNextSteps) > 0 {
		b.WriteString("\nNext:\n")
		for _, step := range result.RecommendedNextSteps {
			fmt.Fprintf(&b, "  %s\n", step)
		}
	}
	return b.String()
}

func tryRiskLabel(risk string) string {
	if risk == "medium" {
		return "MED"
	}
	return strings.ToUpper(risk)
}

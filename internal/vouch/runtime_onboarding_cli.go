package vouch

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/sandbox"
)

const (
	runtimeDoctorVersion = "vouch.runtime_doctor.v0"
	runtimeInitVersion   = "vouch.runtime_init.v0"
	runtimeIgnoreFile    = ".vouch/.gitignore"
)

var runtimeStateIgnore = []byte(`# Local Vouch Runtime state. Keep agent-profiles.json under version control.
kernel.db
kernel.db-shm
kernel.db-wal
kernel.db.lock
vouchd.sock
`)

type runtimeInitResult struct {
	Version           string `json:"version"`
	Repository        string `json:"repository"`
	AgentProfilesPath string `json:"agent_profiles_path"`
	Agent             string `json:"agent"`
	Image             string `json:"image"`
	SourceDigest      string `json:"source_digest"`
	ProfileCreated    bool   `json:"profile_created"`
	IgnorePath        string `json:"ignore_path"`
	IgnoreCreated     bool   `json:"ignore_created"`
	IgnoreUpdated     bool   `json:"ignore_updated"`
}

type runtimeDoctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type runtimeDoctorResult struct {
	Version string               `json:"version"`
	Ready   bool                 `json:"ready"`
	Checks  []runtimeDoctorCheck `json:"checks"`
}

type runtimeDoctorProbes struct {
	gitRoot     func(context.Context, string) (string, error)
	ociEngine   func(context.Context, string) (string, error)
	ociImage    func(context.Context, string, string) error
	daemonReady func(context.Context, string) error
}

func runtimeCommand(
	repo string,
	args []string,
	jsonOut bool,
	stdout, stderr io.Writer,
) int {
	if len(args) == 0 {
		runtimeUsage(stderr)
		return 2
	}
	switch args[0] {
	case "init":
		return runtimeInitCommand(repo, args[1:], jsonOut, stdout, stderr)
	case "doctor":
		return runtimeDoctorCommand(repo, args[1:], jsonOut, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "runtime: unknown command %q\n", args[0])
		runtimeUsage(stderr)
		return 2
	}
}

func runtimeUsage(out io.Writer) {
	fmt.Fprintln(out, "usage: vouch [--repo DIR] [--json] runtime <command>")
	fmt.Fprintln(out, "  runtime init --agent NAME --image IMAGE@sha256:DIGEST --source-digest sha256:DIGEST -- COMMAND [ARG...]")
	fmt.Fprintln(out, "  runtime doctor [--agent NAME] [--runtime-engine ENGINE] [--agent-profiles FILE] [--socket FILE]")
}

func runtimeInitCommand(
	repo string,
	args []string,
	jsonOut bool,
	stdout, stderr io.Writer,
) int {
	flags := flag.NewFlagSet("runtime init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	agent := flags.String("agent", "", "stable name used by vouch run --agent")
	image := flags.String("image", "", "complete digest-pinned OCI image reference")
	sourceDigest := flags.String(
		"source-digest", "",
		"sha256 digest of the agent source",
	)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	command := flags.Args()
	if *agent == "" || *image == "" || *sourceDigest == "" || len(command) == 0 {
		fmt.Fprintln(
			stderr,
			"runtime init requires --agent, --image, --source-digest, and a command after --",
		)
		return 2
	}
	if !model.IsIdentifier(*agent) {
		fmt.Fprintln(stderr, "runtime init: --agent must be a valid identifier")
		return 2
	}
	imageDigest, err := sandbox.ImageDigest(*image)
	if err != nil || !strings.Contains(*image, "@") {
		fmt.Fprintln(
			stderr,
			"runtime init: --image must be a complete IMAGE@sha256:DIGEST reference",
		)
		return 2
	}
	if !model.IsSHA256Digest(*sourceDigest) {
		fmt.Fprintln(stderr, "runtime init: --source-digest must be a lowercase sha256 digest")
		return 2
	}

	root, err := discoverGitRoot(context.Background(), repo)
	if err != nil {
		fmt.Fprintf(stderr, "runtime init: %v\n", err)
		return 1
	}
	sameRoot, err := sameFilesystemPath(root, repo)
	if err != nil {
		fmt.Fprintf(stderr, "runtime init: compare Git repository root: %v\n", err)
		return 1
	}
	if !sameRoot {
		fmt.Fprintf(
			stderr,
			"runtime init: --repo must be the Git repository root (%s)\n",
			root,
		)
		return 1
	}

	profile := agentProfileEntry{
		Name:     *agent,
		OCIImage: *image,
		Descriptor: model.AgentImage{
			Version: model.AgentImageVersion,
			ID:      "image." + *agent + ".v1",
			Digest:  imageDigest,
			Runtime: model.AgentRuntime{
				Adapter:        "oci",
				AdapterVersion: "v1",
				Entrypoint:     append([]string(nil), command...),
			},
			SourceDigest: *sourceDigest,
			Publisher: model.Principal{
				ID:   "service:local-runtime-init",
				Kind: model.PrincipalService,
			},
		},
	}
	document := agentProfileDocument{
		Version: agentProfilesVersion,
		Profiles: []agentProfileEntry{
			profile,
		},
	}
	if err := validateAgentProfileDocument(document); err != nil {
		fmt.Fprintf(stderr, "runtime init: invalid agent profile: %v\n", err)
		return 2
	}

	result, err := writeRuntimeInitialization(
		repo, document, *agent, *image, *sourceDigest,
	)
	if err != nil {
		fmt.Fprintf(stderr, "runtime init: %v\n", err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(result, stdout, stderr)
	}
	if result.ProfileCreated {
		fmt.Fprintf(stdout, "Initialized Vouch Runtime in %s\n", result.Repository)
	} else {
		fmt.Fprintf(stdout, "Vouch Runtime already initialized in %s; profile left unchanged.\n", result.Repository)
	}
	fmt.Fprintf(stdout, "Agent profile: %s (%s)\n", result.Agent, result.AgentProfilesPath)
	if result.IgnoreCreated {
		fmt.Fprintf(stdout, "Runtime state ignore: %s\n", result.IgnorePath)
	} else if result.IgnoreUpdated {
		fmt.Fprintf(stdout, "Added missing Runtime state rules to %s\n", result.IgnorePath)
	} else {
		fmt.Fprintf(stdout, "Runtime state ignore is complete: %s\n", result.IgnorePath)
	}
	fmt.Fprintf(stdout, "Next: vouch --repo %s doctor\n", result.Repository)
	return 0
}

func writeRuntimeInitialization(
	repo string,
	document agentProfileDocument,
	agent, image, sourceDigest string,
) (runtimeInitResult, error) {
	vouchDirectory := filepath.Join(repo, ".vouch")
	info, err := os.Lstat(vouchDirectory)
	switch {
	case err == nil:
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return runtimeInitResult{}, errors.New(".vouch must be a real directory")
		}
	case os.IsNotExist(err):
		if err := os.Mkdir(vouchDirectory, 0o700); err != nil {
			return runtimeInitResult{}, fmt.Errorf("create .vouch directory: %w", err)
		}
	default:
		return runtimeInitResult{}, fmt.Errorf("inspect .vouch directory: %w", err)
	}

	profilesPath := filepath.Join(repo, defaultAgentProfiles)
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return runtimeInitResult{}, fmt.Errorf("encode agent profiles: %w", err)
	}
	data = append(data, '\n')
	profileCreated := false
	if info, err := os.Lstat(profilesPath); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return runtimeInitResult{}, fmt.Errorf(
				"%s exists but is not a regular file",
				defaultAgentProfiles,
			)
		}
		existingData, err := readAgentProfilesFile(profilesPath)
		if err != nil {
			return runtimeInitResult{}, err
		}
		existing, err := decodeAgentProfileDocument(existingData)
		if err != nil || !reflect.DeepEqual(existing, document) {
			return runtimeInitResult{}, fmt.Errorf(
				"%s already exists with different content; refusing to overwrite it",
				defaultAgentProfiles,
			)
		}
	} else if os.IsNotExist(err) {
		if err := writeExclusiveRegularFile(profilesPath, data, 0o600); err != nil {
			return runtimeInitResult{}, fmt.Errorf("create %s: %w", defaultAgentProfiles, err)
		}
		profileCreated = true
	} else {
		return runtimeInitResult{}, fmt.Errorf("inspect %s: %w", defaultAgentProfiles, err)
	}

	ignorePath := filepath.Join(repo, runtimeIgnoreFile)
	ignoreCreated := false
	ignoreUpdated := false
	if info, err := os.Lstat(ignorePath); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			if profileCreated {
				_ = os.Remove(profilesPath)
			}
			return runtimeInitResult{}, fmt.Errorf(
				"%s exists but is not a regular file",
				runtimeIgnoreFile,
			)
		}
		existing, err := os.ReadFile(ignorePath)
		if err != nil {
			if profileCreated {
				_ = os.Remove(profilesPath)
			}
			return runtimeInitResult{}, fmt.Errorf("read %s: %w", runtimeIgnoreFile, err)
		}
		missing := missingRuntimeIgnoreEntries(existing)
		if len(missing) > 0 {
			if err := appendRuntimeIgnoreEntries(ignorePath, existing, missing); err != nil {
				if profileCreated {
					_ = os.Remove(profilesPath)
				}
				return runtimeInitResult{}, fmt.Errorf("update %s: %w", runtimeIgnoreFile, err)
			}
			ignoreUpdated = true
		}
	} else if os.IsNotExist(err) {
		if err := writeExclusiveRegularFile(ignorePath, runtimeStateIgnore, 0o600); err != nil {
			if profileCreated {
				_ = os.Remove(profilesPath)
			}
			return runtimeInitResult{}, fmt.Errorf("create %s: %w", runtimeIgnoreFile, err)
		}
		ignoreCreated = true
	} else {
		if profileCreated {
			_ = os.Remove(profilesPath)
		}
		return runtimeInitResult{}, fmt.Errorf("inspect %s: %w", runtimeIgnoreFile, err)
	}

	return runtimeInitResult{
		Version:           runtimeInitVersion,
		Repository:        repo,
		AgentProfilesPath: profilesPath,
		Agent:             agent,
		Image:             image,
		SourceDigest:      sourceDigest,
		ProfileCreated:    profileCreated,
		IgnorePath:        ignorePath,
		IgnoreCreated:     ignoreCreated,
		IgnoreUpdated:     ignoreUpdated,
	}, nil
}

func missingRuntimeIgnoreEntries(data []byte) []string {
	lines := make(map[string]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			lines[line] = struct{}{}
		}
	}
	var missing []string
	for _, entry := range []string{
		"kernel.db", "kernel.db-shm", "kernel.db-wal", "kernel.db.lock", "vouchd.sock",
	} {
		if _, exists := lines[entry]; exists {
			continue
		}
		if _, exists := lines["/"+entry]; exists {
			continue
		}
		missing = append(missing, entry)
	}
	return missing
}

func appendRuntimeIgnoreEntries(path string, existing []byte, missing []string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	var addition strings.Builder
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		addition.WriteByte('\n')
	}
	addition.WriteString("# Local Vouch Runtime state.\n")
	for _, entry := range missing {
		addition.WriteString(entry)
		addition.WriteByte('\n')
	}
	if _, err := io.WriteString(file, addition.String()); err != nil {
		return err
	}
	return file.Sync()
}

func writeExclusiveRegularFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

func runtimeDoctorCommand(
	repo string,
	args []string,
	jsonOut bool,
	stdout, stderr io.Writer,
) int {
	return runtimeDoctorCommandWithProbes(
		repo, args, jsonOut, stdout, stderr, runtimeDoctorProbes{},
	)
}

func runtimeDoctorCommandWithProbes(
	repo string,
	args []string,
	jsonOut bool,
	stdout, stderr io.Writer,
	probes runtimeDoctorProbes,
) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	engine := flags.String("runtime-engine", "docker", "OCI engine executable")
	agent := flags.String("agent", "", "agent profile that must be executable")
	profilesPath := flags.String(
		"agent-profiles", defaultAgentProfiles,
		"agent profile document",
	)
	socketPath := flags.String("socket", defaultKernelSocket(repo), "vouchd Unix socket")
	timeout := flags.Duration("timeout", 5*time.Second, "timeout for each external readiness check")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "doctor: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if strings.TrimSpace(*engine) == "" {
		fmt.Fprintln(stderr, "doctor: --runtime-engine cannot be empty")
		return 2
	}
	if *agent != "" && !model.IsIdentifier(*agent) {
		fmt.Fprintln(stderr, "doctor: --agent must be a valid identifier")
		return 2
	}
	if *timeout <= 0 || *timeout > time.Minute {
		fmt.Fprintln(stderr, "doctor: --timeout must be greater than zero and at most 1m")
		return 2
	}
	if !filepath.IsAbs(*profilesPath) {
		*profilesPath = filepath.Join(repo, *profilesPath)
	}
	if !filepath.IsAbs(*socketPath) {
		*socketPath = filepath.Join(repo, *socketPath)
	}
	probes = withDefaultRuntimeDoctorProbes(probes)
	result := runRuntimeDoctor(
		repo, *engine, *agent, *profilesPath, *socketPath, *timeout, probes,
	)
	if jsonOut {
		if code := renderCommandJSON(result, stdout, stderr); code != 0 {
			return code
		}
	} else {
		fmt.Fprintln(stdout, "Vouch Runtime doctor")
		for _, check := range result.Checks {
			fmt.Fprintf(
				stdout, "[%s] %s: %s\n",
				strings.ToUpper(check.Status), check.Name, check.Message,
			)
		}
		if result.Ready {
			fmt.Fprintln(stdout, "Ready: required local Runtime checks passed.")
		} else {
			fmt.Fprintln(stdout, "Not ready: fix the failed checks above.")
		}
	}
	if !result.Ready {
		return 1
	}
	return 0
}

func withDefaultRuntimeDoctorProbes(probes runtimeDoctorProbes) runtimeDoctorProbes {
	if probes.gitRoot == nil {
		probes.gitRoot = discoverGitRoot
	}
	if probes.ociEngine == nil {
		probes.ociEngine = probeOCIEngine
	}
	if probes.ociImage == nil {
		probes.ociImage = probeOCIImage
	}
	if probes.daemonReady == nil {
		probes.daemonReady = probeDaemonReadiness
	}
	return probes
}

func runRuntimeDoctor(
	repo, engine, selectedAgent, profilesPath, socketPath string,
	timeout time.Duration,
	probes runtimeDoctorProbes,
) runtimeDoctorResult {
	result := runtimeDoctorResult{Version: runtimeDoctorVersion, Ready: true}
	add := func(name, status, message string) {
		result.Checks = append(result.Checks, runtimeDoctorCheck{
			Name: name, Status: status, Message: message,
		})
		if status == "fail" {
			result.Ready = false
		}
	}
	probeContext := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), timeout)
	}

	ctx, cancel := probeContext()
	root, err := probes.gitRoot(ctx, repo)
	cancel()
	if err != nil {
		add("git_repository", "fail", err.Error())
	} else {
		sameRoot, compareErr := sameFilesystemPath(root, repo)
		switch {
		case compareErr != nil:
			add("git_repository", "fail", "compare Git repository root: "+compareErr.Error())
		case !sameRoot:
			add("git_repository", "fail", "--repo must be the Git repository root: "+root)
		default:
			add("git_repository", "pass", "Git worktree detected at "+root)
		}
	}

	ctx, cancel = probeContext()
	enginePath, engineErr := probes.ociEngine(ctx, engine)
	cancel()
	if engineErr != nil {
		add("oci_engine", "fail", engineErr.Error())
	} else {
		add("oci_engine", "pass", engine+" is available and reachable at "+enginePath)
	}

	var profiles agentProfileDocument
	profilesValid := false
	data, profileErr := readAgentProfilesFile(profilesPath)
	if profileErr == nil {
		profiles, profileErr = decodeAgentProfileDocument(data)
	}
	switch {
	case profileErr == nil:
		profilesValid = true
		add(
			"agent_profiles", "pass",
			fmt.Sprintf("%d valid profile(s) in %s", len(profiles.Profiles), profilesPath),
		)
	case os.IsNotExist(profileRootCause(profileErr)):
		add(
			"agent_profiles", "warn",
			"not configured; create it with `vouch runtime init`",
		)
	default:
		add("agent_profiles", "fail", profileErr.Error())
	}

	switch {
	case !profilesValid:
		if selectedAgent == "" {
			add("agent_images", "warn", "not checked because agent profiles are unavailable")
		} else {
			add(
				"agent_images", "fail",
				fmt.Sprintf("agent profile %q cannot be checked because profiles are unavailable", selectedAgent),
			)
		}
	case engineErr != nil:
		add("agent_images", "warn", "not checked because the OCI engine is unavailable")
	default:
		profileImages := profileImagesByName(profiles)
		selectedImage, selectedExists := profileImages[selectedAgent]
		switch {
		case selectedAgent == "":
			add(
				"agent_images", "warn",
				"not checked; pass --agent NAME to verify one pull=never image",
			)
		case !selectedExists:
			add(
				"agent_images", "fail",
				fmt.Sprintf("agent profile %q was not found in %s", selectedAgent, profilesPath),
			)
		default:
			ctx, cancel = probeContext()
			err := probes.ociImage(ctx, enginePath, selectedImage)
			cancel()
			if err != nil {
				add(
					"agent_images", "fail",
					fmt.Sprintf(
						"selected agent %q image is not present locally: %s; Vouch executes with pull=never",
						selectedAgent, selectedImage,
					),
				)
			} else {
				add(
					"agent_images", "pass",
					fmt.Sprintf("selected agent %q image is present locally", selectedAgent),
				)
			}
		}
	}

	socketInfo, socketErr := os.Lstat(socketPath)
	switch {
	case os.IsNotExist(socketErr):
		add(
			"daemon", "warn",
			"not running; start it with `vouch daemon` when you are ready to execute",
		)
	case socketErr != nil:
		add("daemon", "fail", "inspect daemon socket: "+socketErr.Error())
	case socketInfo.Mode()&os.ModeSymlink != 0 || socketInfo.Mode()&os.ModeSocket == 0:
		add("daemon", "fail", "daemon path exists but is not a real Unix socket: "+socketPath)
	case socketInfo.Mode().Perm()&0o077 != 0:
		add("daemon", "fail", "daemon socket permissions must not grant group or other access: "+socketPath)
	default:
		ctx, cancel = probeContext()
		err := probes.daemonReady(ctx, socketPath)
		cancel()
		if err != nil {
			add("daemon", "fail", err.Error())
		} else {
			add("daemon", "pass", "vouchd is ready at "+socketPath)
		}
	}
	return result
}

func profileRootCause(err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err
		}
		err = next
	}
}

func profileImagesByName(document agentProfileDocument) map[string]string {
	images := make(map[string]string, len(document.Profiles))
	for _, profile := range document.Profiles {
		images[profile.Name] = profile.OCIImage
	}
	return images
}

func discoverGitRoot(ctx context.Context, repo string) (string, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("find Git executable: %w", err)
	}
	command := exec.CommandContext(ctx, gitPath, "-C", repo, "rev-parse", "--show-toplevel")
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("%s is not a Git worktree", repo)
	}
	root := strings.TrimSpace(string(output))
	if root == "" {
		return "", fmt.Errorf("%s is not a Git worktree", repo)
	}
	command = exec.CommandContext(ctx, gitPath, "-C", repo, "rev-parse", "--verify", "HEAD^{commit}")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("%s does not have a HEAD commit", repo)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve Git root: %w", err)
	}
	return absolute, nil
}

func sameFilesystemPath(left, right string) (bool, error) {
	leftResolved, err := filepath.EvalSymlinks(left)
	if err != nil {
		return false, err
	}
	rightResolved, err := filepath.EvalSymlinks(right)
	if err != nil {
		return false, err
	}
	return filepath.Clean(leftResolved) == filepath.Clean(rightResolved), nil
}

func probeOCIEngine(ctx context.Context, engine string) (string, error) {
	path, err := exec.LookPath(engine)
	if err != nil {
		return "", fmt.Errorf("find OCI engine %q: %w", engine, err)
	}
	command := exec.CommandContext(ctx, path, "info", "--format", "{{.ServerVersion}}")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("OCI engine %q readiness timed out", engine)
		}
		return "", fmt.Errorf("OCI engine %q is installed but unreachable: %w", engine, err)
	}
	return path, nil
}

func probeOCIImage(ctx context.Context, enginePath, image string) error {
	command := exec.CommandContext(
		ctx, enginePath, "image", "inspect", "--format", "{{.Id}}", image,
	)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}

func probeDaemonReadiness(ctx context.Context, socketPath string) error {
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, "http://vouchd/readyz", nil,
	)
	if err != nil {
		return fmt.Errorf("build vouchd readiness request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("vouchd socket exists but readiness failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("vouchd is not ready: %s", response.Status)
	}
	var body struct {
		Status string `json:"status"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4097))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return fmt.Errorf("decode vouchd readiness response: %w", err)
	}
	if body.Status != "ready" {
		return fmt.Errorf("vouchd returned unexpected readiness status %q", body.Status)
	}
	return nil
}

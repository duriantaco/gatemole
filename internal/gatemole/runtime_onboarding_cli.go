package gatemole

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	kernelclient "github.com/duriantaco/gatemole/internal/kernel/client"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/runtimeidentity"
	"github.com/duriantaco/gatemole/internal/kernel/runtimepreflight"
	"github.com/duriantaco/gatemole/internal/kernel/sandbox"
)

const (
	runtimeDoctorVersion = "gatemole.runtime_doctor.v0"
	runtimeInitVersion   = "gatemole.runtime_init.v0"
	runtimeIgnoreFile    = ".gatemole/.gitignore"
)

var runtimeStateIgnore = []byte(`# Local Gatemole Runtime state. Keep agent-profiles.json under version control.
kernel.db
kernel.db-journal
kernel.db-shm
kernel.db-wal
kernel.db.lock
runtime.lock
gatemoled.sock
runtime.json
`)

type runtimeInitResult struct {
	Version                string `json:"version"`
	Repository             string `json:"repository"`
	AgentProfilesPath      string `json:"agent_profiles_path"`
	Agent                  string `json:"agent"`
	Image                  string `json:"image"`
	SourceDigest           string `json:"source_digest"`
	RuntimeID              string `json:"runtime_id"`
	RuntimeIdentityPath    string `json:"runtime_identity_path"`
	ProfileCreated         bool   `json:"profile_created"`
	RuntimeIdentityCreated bool   `json:"runtime_identity_created"`
	IgnorePath             string `json:"ignore_path"`
	IgnoreCreated          bool   `json:"ignore_created"`
	IgnoreUpdated          bool   `json:"ignore_updated"`
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
	gitRoot         func(context.Context, string) (string, error)
	ociEngine       func(context.Context, string) (string, error)
	ociImage        func(context.Context, string, string) error
	socketInfo      func(string) (os.FileInfo, error)
	daemonPreflight func(
		context.Context,
		string,
		string,
		runtimepreflight.Request,
	) (runtimepreflight.Result, error)
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
	fmt.Fprintln(out, "usage: gatemole [--repo DIR] [--json] runtime <command>")
	fmt.Fprintln(out, "  runtime init [--agent NAME --image IMAGE@sha256:DIGEST --source-digest sha256:DIGEST -- COMMAND [ARG...]]")
	fmt.Fprintln(out, "  runtime doctor [--agent NAME] [--namespace NAME] [--require-enforcement-profile PROFILE] [--runtime-engine ENGINE] [--agent-profiles FILE] [--socket FILE]")
	fmt.Fprintln(out, "  omit all runtime init profile inputs to initialize only the repository-local Runtime identity")
	fmt.Fprintln(out, "  PROFILE must be development or production")
}

func runtimeInitCommand(
	repo string,
	args []string,
	jsonOut bool,
	stdout, stderr io.Writer,
) int {
	flags := flag.NewFlagSet("runtime init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	agent := flags.String("agent", "", "stable name used by gatemole run --agent")
	image := flags.String("image", "", "complete digest-pinned OCI image reference")
	sourceDigest := flags.String(
		"source-digest", "",
		"sha256 digest of the agent source",
	)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	command := flags.Args()
	profileRequested := *agent != "" ||
		*image != "" ||
		*sourceDigest != "" ||
		len(command) > 0
	if profileRequested &&
		(*agent == "" ||
			*image == "" ||
			*sourceDigest == "" ||
			len(command) == 0) {
		fmt.Fprintln(
			stderr,
			"runtime init profile registration requires --agent, --image, --source-digest, and a command after --; omit all profile inputs to initialize identity only",
		)
		return 2
	}

	var document *agentProfileDocument
	if profileRequested {
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
		candidate := agentProfileDocument{
			Version:  agentProfilesVersion,
			Profiles: []agentProfileEntry{profile},
		}
		if err := validateAgentProfileDocument(candidate); err != nil {
			fmt.Fprintf(stderr, "runtime init: invalid agent profile: %v\n", err)
			return 2
		}
		document = &candidate
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
	trackedState, err := trackedRuntimeState(context.Background(), repo)
	if err != nil {
		fmt.Fprintf(stderr, "runtime init: inspect tracked Runtime state: %v\n", err)
		return 1
	}
	if len(trackedState) > 0 {
		fmt.Fprintf(
			stderr,
			"runtime init: local Runtime state must not be tracked by Git: %s\n",
			strings.Join(trackedState, ", "),
		)
		return 1
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
	if result.RuntimeIdentityCreated || result.ProfileCreated {
		fmt.Fprintf(stdout, "Initialized Gatemole Runtime in %s\n", result.Repository)
	} else if result.Agent == "" {
		fmt.Fprintf(stdout, "Gatemole Runtime already initialized in %s.\n", result.Repository)
	} else {
		fmt.Fprintf(stdout, "Gatemole Runtime already initialized in %s; profile left unchanged.\n", result.Repository)
	}
	fmt.Fprintf(
		stdout,
		"Runtime instance: %s (%s; local-only)\n",
		result.RuntimeID,
		result.RuntimeIdentityPath,
	)
	if result.Agent != "" {
		fmt.Fprintf(stdout, "Agent profile: %s (%s)\n", result.Agent, result.AgentProfilesPath)
	}
	if result.IgnoreCreated {
		fmt.Fprintf(stdout, "Runtime state ignore: %s\n", result.IgnorePath)
	} else if result.IgnoreUpdated {
		fmt.Fprintf(stdout, "Added missing Runtime state rules to %s\n", result.IgnorePath)
	} else {
		fmt.Fprintf(stdout, "Runtime state ignore is complete: %s\n", result.IgnorePath)
	}
	fmt.Fprintf(stdout, "Next: gatemole --repo %s doctor\n", result.Repository)
	return 0
}

func writeRuntimeInitialization(
	repo string,
	document *agentProfileDocument,
	agent, image, sourceDigest string,
) (runtimeInitResult, error) {
	if err := runtimeidentity.EnsurePrivateDirectory(
		context.Background(),
		repo,
	); err != nil {
		return runtimeInitResult{}, err
	}

	identityPath := filepath.Join(
		repo,
		filepath.FromSlash(runtimeidentity.IdentityRelativePath),
	)
	if _, err := os.Lstat(identityPath); err == nil {
		if _, err := runtimeidentity.Load(
			context.Background(),
			repo,
		); err != nil {
			return runtimeInitResult{}, fmt.Errorf(
				"validate existing Runtime identity before initialization: %w",
				err,
			)
		}
	} else if !os.IsNotExist(err) {
		return runtimeInitResult{}, fmt.Errorf(
			"inspect existing Runtime identity: %w",
			err,
		)
	}

	profilesPath := filepath.Join(repo, defaultAgentProfiles)
	profileCreated := false
	if document != nil {
		data, err := json.MarshalIndent(*document, "", "  ")
		if err != nil {
			return runtimeInitResult{}, fmt.Errorf("encode agent profiles: %w", err)
		}
		data = append(data, '\n')
		if info, err := os.Lstat(profilesPath); err == nil {
			if !info.Mode().IsRegular() ||
				info.Mode()&os.ModeSymlink != 0 {
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
			if err != nil ||
				!reflect.DeepEqual(existing, *document) {
				return runtimeInitResult{}, fmt.Errorf(
					"%s already exists with different content; refusing to overwrite it",
					defaultAgentProfiles,
				)
			}
		} else if os.IsNotExist(err) {
			if err := writeExclusiveRegularFile(
				profilesPath,
				data,
				0o600,
			); err != nil {
				return runtimeInitResult{}, fmt.Errorf(
					"create %s: %w",
					defaultAgentProfiles,
					err,
				)
			}
			profileCreated = true
		} else {
			return runtimeInitResult{}, fmt.Errorf(
				"inspect %s: %w",
				defaultAgentProfiles,
				err,
			)
		}
	}

	ignorePath := filepath.Join(repo, runtimeIgnoreFile)
	ignoreCreated := false
	ignoreUpdated := false
	var originalIgnore []byte
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
		originalIgnore = append([]byte(nil), existing...)
		missing := missingRuntimeIgnoreEntries(existing)
		if len(missing) > 0 {
			if err := appendRuntimeIgnoreEntries(
				ignorePath,
				info,
				existing,
				missing,
			); err != nil {
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

	identity, identityCreated, err := runtimeidentity.CreateOrLoad(
		context.Background(),
		repo,
	)
	if err != nil {
		if profileCreated {
			_ = os.Remove(profilesPath)
		}
		switch {
		case ignoreCreated:
			_ = os.Remove(ignorePath)
		case ignoreUpdated:
			_ = os.WriteFile(ignorePath, originalIgnore, 0o600)
		}
		return runtimeInitResult{}, fmt.Errorf("initialize Runtime identity: %w", err)
	}

	return runtimeInitResult{
		Version:           runtimeInitVersion,
		Repository:        repo,
		AgentProfilesPath: profilesPath,
		Agent:             agent,
		Image:             image,
		SourceDigest:      sourceDigest,
		RuntimeID:         identity.RuntimeID,
		RuntimeIdentityPath: filepath.Join(
			repo,
			filepath.FromSlash(runtimeidentity.IdentityRelativePath),
		),
		ProfileCreated:         profileCreated,
		RuntimeIdentityCreated: identityCreated,
		IgnorePath:             ignorePath,
		IgnoreCreated:          ignoreCreated,
		IgnoreUpdated:          ignoreUpdated,
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
		"kernel.db", "kernel.db-journal", "kernel.db-shm", "kernel.db-wal",
		"kernel.db.lock", "runtime.lock", "gatemoled.sock", "runtime.json",
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

func appendRuntimeIgnoreEntries(
	path string,
	expected os.FileInfo,
	existing []byte,
	missing []string,
) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() ||
		expected == nil ||
		!os.SameFile(expected, opened) {
		return errors.New(
			"Runtime ignore changed before it could be updated",
		)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() ||
		current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(opened, current) {
		return errors.New(
			"Runtime ignore changed while it was opened",
		)
	}
	var addition strings.Builder
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		addition.WriteByte('\n')
	}
	if !bytes.Contains(
		existing,
		[]byte("# Local Gatemole Runtime state."),
	) {
		addition.WriteString("# Local Gatemole Runtime state.\n")
	}
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
	namespace := flags.String("namespace", "local", "daemon namespace to preflight")
	requiredEnforcementProfile := flags.String(
		"require-enforcement-profile",
		"",
		"require daemon enforcement profile: development or production",
	)
	profilesPath := flags.String(
		"agent-profiles", defaultAgentProfiles,
		"agent profile document",
	)
	socketPath := flags.String("socket", defaultKernelSocket(repo), "gatemoled Unix socket")
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
	if !model.IsIdentifier(*namespace) {
		fmt.Fprintln(stderr, "doctor: --namespace must be a valid identifier")
		return 2
	}
	if *requiredEnforcementProfile != "" &&
		!model.IsEnforcementProfile(*requiredEnforcementProfile) {
		fmt.Fprintln(
			stderr,
			"doctor: --require-enforcement-profile must be development or production",
		)
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
		repo,
		*engine,
		*agent,
		*namespace,
		*requiredEnforcementProfile,
		*profilesPath,
		*socketPath,
		*timeout,
		probes,
	)
	if jsonOut {
		if code := renderCommandJSON(result, stdout, stderr); code != 0 {
			return code
		}
	} else {
		fmt.Fprintln(stdout, "Gatemole Runtime doctor")
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
	if probes.socketInfo == nil {
		probes.socketInfo = os.Lstat
	}
	if probes.daemonPreflight == nil {
		probes.daemonPreflight = probeDaemonRuntime
	}
	return probes
}

func runRuntimeDoctor(
	repo, engine, selectedAgent, namespace, requiredEnforcementProfile,
	profilesPath, socketPath string,
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
	gitRepositoryValid := false
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
			gitRepositoryValid = true
			add("git_repository", "pass", "Git worktree detected at "+root)
		}
	}

	var identity runtimeidentity.Identity
	var identityErr error
	if gitRepositoryValid {
		ctx, cancel = probeContext()
		trackedState, trackedErr := trackedRuntimeState(ctx, repo)
		cancel()
		switch {
		case trackedErr != nil:
			add(
				"runtime_state",
				"fail",
				"inspect tracked local Runtime state: "+trackedErr.Error(),
			)
		case len(trackedState) > 0:
			add(
				"runtime_state",
				"fail",
				"local Runtime state is tracked by Git: "+
					strings.Join(trackedState, ", "),
			)
		default:
			add("runtime_state", "pass", "local Runtime state is not tracked by Git")
		}

		ctx, cancel = probeContext()
		identity, identityErr = runtimeidentity.Load(ctx, repo)
		cancel()
		if identityErr != nil {
			add(
				"runtime_identity",
				"fail",
				"load local Runtime identity: "+identityErr.Error()+
					"; run `gatemole runtime init`",
			)
		} else {
			add(
				"runtime_identity",
				"pass",
				"local Runtime instance is "+identity.RuntimeID,
			)
		}
	} else {
		identityErr = errors.New("Git repository check failed")
		add(
			"runtime_state",
			"fail",
			"not checked because the Git repository check failed",
		)
		add(
			"runtime_identity",
			"fail",
			"not checked because the Git repository check failed",
		)
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
			"not configured; create it with `gatemole runtime init`",
		)
	default:
		add("agent_profiles", "fail", profileErr.Error())
	}

	var selectedPreflight *runtimepreflight.AgentSelection
	var selectedProfileErr error
	if profilesValid && selectedAgent != "" {
		selectedPreflight, selectedProfileErr = runtimeDoctorAgentSelection(
			profiles,
			selectedAgent,
		)
	}

	socketSecure := false
	socketStatus := ""
	socketMessage := ""
	socketInfo, socketErr := probes.socketInfo(socketPath)
	switch {
	case os.IsNotExist(socketErr):
		socketStatus = "warn"
		socketMessage =
			"not running; start it with `gatemole daemon` when you are ready to execute"
	case socketErr != nil:
		socketStatus = "fail"
		socketMessage = "inspect daemon socket: " + socketErr.Error()
	case socketInfo.Mode()&os.ModeSymlink != 0 || socketInfo.Mode()&os.ModeSocket == 0:
		socketStatus = "fail"
		socketMessage = "daemon path exists but is not a real Unix socket: " + socketPath
	case socketInfo.Mode().Perm()&0o077 != 0:
		socketStatus = "fail"
		socketMessage =
			"daemon socket permissions must not grant group or other access: " +
				socketPath
	default:
		socketSecure = true
	}

	if socketSecure {
		runDaemonDoctorChecks(
			add,
			probeContext,
			probes,
			socketPath,
			namespace,
			requiredEnforcementProfile,
			identity,
			identityErr,
			selectedAgent,
			profilesValid,
			selectedPreflight,
			selectedProfileErr,
		)
		return result
	}

	if requiredEnforcementProfile != "" {
		add(
			"enforcement_profile",
			"fail",
			"cannot verify required daemon enforcement profile "+
				requiredEnforcementProfile+" because no secure daemon socket is available",
		)
	} else {
		add(
			"enforcement_profile",
			"warn",
			"not checked because no secure daemon socket is available",
		)
	}

	ctx, cancel = probeContext()
	enginePath, engineErr := probes.ociEngine(ctx, engine)
	cancel()
	if engineErr != nil {
		add("oci_engine", "fail", engineErr.Error())
	} else {
		add("oci_engine", "pass", engine+" is available and reachable at "+enginePath)
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
						"selected agent %q image is not present locally: %s; Gatemole executes with pull=never",
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

	add("daemon", socketStatus, socketMessage)
	return result
}

func runDaemonDoctorChecks(
	add func(string, string, string),
	probeContext func() (context.Context, context.CancelFunc),
	probes runtimeDoctorProbes,
	socketPath string,
	namespace string,
	requiredEnforcementProfile string,
	identity runtimeidentity.Identity,
	identityErr error,
	selectedAgent string,
	profilesValid bool,
	selected *runtimepreflight.AgentSelection,
	selectedErr error,
) {
	if identityErr != nil {
		add(
			"enforcement_profile",
			"fail",
			"daemon enforcement profile was not checked because the local Runtime identity is unavailable",
		)
		add(
			"oci_engine",
			"fail",
			"daemon-owned OCI engine was not checked because the local Runtime identity is unavailable",
		)
		addRuntimeDoctorAgentImageCheck(
			add,
			selectedAgent,
			profilesValid,
			selected,
			selectedErr,
			errors.New("local Runtime identity is unavailable"),
		)
		add(
			"daemon",
			"fail",
			"daemon preflight requires a valid local Runtime identity",
		)
		return
	}

	request := runtimepreflight.Request{
		Version:                    runtimepreflight.RequestVersion,
		ExpectedRuntimeID:          identity.RuntimeID,
		RequiredEnforcementProfile: requiredEnforcementProfile,
		Agent:                      selected,
	}
	ctx, cancel := probeContext()
	preflight, err := probes.daemonPreflight(
		ctx,
		socketPath,
		namespace,
		request,
	)
	cancel()
	if err != nil {
		add(
			"enforcement_profile",
			"fail",
			"daemon enforcement profile was not established: "+err.Error(),
		)
		addRuntimeDoctorEngineCheck(add, err)
		addRuntimeDoctorAgentImageCheck(
			add,
			selectedAgent,
			profilesValid,
			selected,
			selectedErr,
			err,
		)
		add("daemon", "fail", "authoritative Runtime preflight failed: "+err.Error())
		return
	}
	if preflight.RuntimeID != identity.RuntimeID {
		mismatch := errors.New("daemon returned a different Runtime identity")
		add(
			"enforcement_profile",
			"fail",
			"daemon enforcement profile was not established",
		)
		addRuntimeDoctorEngineCheck(add, mismatch)
		addRuntimeDoctorAgentImageCheck(
			add,
			selectedAgent,
			profilesValid,
			selected,
			selectedErr,
			mismatch,
		)
		add("daemon", "fail", mismatch.Error())
		return
	}

	add(
		"enforcement_profile",
		"pass",
		"daemon enforcement profile is "+preflight.EnforcementProfile,
	)
	add(
		"oci_engine",
		"pass",
		"daemon-owned OCI engine is available and reachable",
	)
	addRuntimeDoctorAgentImageCheck(
		add,
		selectedAgent,
		profilesValid,
		selected,
		selectedErr,
		nil,
	)
	add(
		"daemon",
		"pass",
		fmt.Sprintf(
			"gatemoled accepted Runtime %s using the %s enforcement profile",
			preflight.RuntimeID,
			preflight.EnforcementProfile,
		),
	)
}

func addRuntimeDoctorEngineCheck(
	add func(string, string, string),
	preflightErr error,
) {
	var kernelErr *model.KernelError
	if errors.As(preflightErr, &kernelErr) &&
		kernelErr.Code == model.ErrorDriverUnavailable {
		add(
			"oci_engine",
			"fail",
			"daemon reported its Runtime engine or ledger unavailable",
		)
		return
	}
	add(
		"oci_engine",
		"warn",
		"not checked because authoritative preflight was rejected before engine readiness was established",
	)
}

func addRuntimeDoctorAgentImageCheck(
	add func(string, string, string),
	selectedAgent string,
	profilesValid bool,
	selected *runtimepreflight.AgentSelection,
	selectedErr error,
	preflightErr error,
) {
	switch {
	case !profilesValid && selectedAgent == "":
		add("agent_images", "warn", "not checked because agent profiles are unavailable")
	case !profilesValid:
		add(
			"agent_images",
			"fail",
			fmt.Sprintf(
				"agent profile %q cannot be checked because profiles are unavailable",
				selectedAgent,
			),
		)
	case selectedAgent == "":
		add(
			"agent_images",
			"warn",
			"not checked; pass --agent NAME to verify one daemon-owned pull=never image",
		)
	case selectedErr != nil:
		add("agent_images", "fail", selectedErr.Error())
	case selected == nil:
		add(
			"agent_images",
			"fail",
			fmt.Sprintf("agent profile %q was not found", selectedAgent),
		)
	case preflightErr != nil:
		add(
			"agent_images",
			"fail",
			fmt.Sprintf(
				"daemon preflight did not accept selected agent %q: %v",
				selectedAgent,
				preflightErr,
			),
		)
	default:
		add(
			"agent_images",
			"pass",
			fmt.Sprintf(
				"daemon verified selected agent %q image with pull=never",
				selectedAgent,
			),
		)
	}
}

func runtimeDoctorAgentSelection(
	document agentProfileDocument,
	name string,
) (*runtimepreflight.AgentSelection, error) {
	for _, profile := range document.Profiles {
		if profile.Name != name {
			continue
		}
		digest, err := canonicalAgentProfileDigest(profile)
		if err != nil {
			return nil, err
		}
		if len(profile.Descriptor.Runtime.Entrypoint) == 0 {
			return nil, fmt.Errorf("agent profile %q has no entrypoint", name)
		}
		resolved := resolvedAgentProfile{
			ID:           profile.Name,
			Digest:       digest,
			RuntimeClass: "oci",
			OCIImage:     profile.OCIImage,
			ImageDigest:  profile.Descriptor.Digest,
			Entrypoint:   profile.Descriptor.Runtime.Entrypoint[0],
			Command: append(
				[]string(nil),
				profile.Descriptor.Runtime.Entrypoint...,
			),
		}
		binding, err := resolved.binding()
		if err != nil {
			return nil, err
		}
		return &runtimepreflight.AgentSelection{
			Profile:  binding,
			OCIImage: profile.OCIImage,
		}, nil
	}
	return nil, fmt.Errorf("agent profile %q was not found", name)
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

func probeDaemonRuntime(
	ctx context.Context,
	socketPath string,
	namespace string,
	request runtimepreflight.Request,
) (runtimepreflight.Result, error) {
	return kernelclient.New(socketPath).
		WithExpectedRuntimeID(request.ExpectedRuntimeID).
		PreflightRuntime(
			ctx,
			namespace,
			request,
		)
}

var runtimeLocalStatePaths = []string{
	".gatemole/runtime.json",
	".gatemole/kernel.db",
	".gatemole/kernel.db-journal",
	".gatemole/kernel.db-shm",
	".gatemole/kernel.db-wal",
	".gatemole/kernel.db.lock",
	".gatemole/runtime.lock",
	".gatemole/gatemoled.sock",
}

func trackedRuntimeState(
	ctx context.Context,
	repositoryRoot string,
) ([]string, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("find Git executable: %w", err)
	}
	args := []string{
		"-C",
		repositoryRoot,
		"ls-files",
		"--cached",
		"-z",
		"--",
	}
	args = append(args, runtimeLocalStatePaths...)
	command := exec.CommandContext(ctx, gitPath, args...)
	output, err := command.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("inspect Git index: %w", err)
	}
	var tracked []string
	for _, path := range strings.Split(string(output), "\x00") {
		if path != "" {
			tracked = append(tracked, path)
		}
	}
	return tracked, nil
}

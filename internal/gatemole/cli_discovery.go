package gatemole

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
)

const cliVersionSchema = "gatemole.cli_version.v1"

// These values may be set by release builds with -ldflags -X. Development
// builds fall back to the module and VCS metadata embedded by the Go tool.
var (
	buildVersion = "dev"
	buildCommit  = ""
)

type cliVersionInfo struct {
	Schema    string `json:"schema"`
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Dirty     bool   `json:"dirty"`
	GoVersion string `json:"go_version"`
}

// renderRequestedHelp handles discovery before any repository or Runtime state
// is inspected. It deliberately stops looking at the command separator so an
// agent may still receive -h or --help as an argument.
func renderRequestedHelp(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	if args[0] == "-h" || args[0] == "--help" {
		usage(stdout)
		return true, 0
	}
	if args[0] == "help" {
		if len(args) == 1 {
			usage(stdout)
			return true, 0
		}
		if len(args) != 2 {
			fmt.Fprintln(stderr, "help accepts at most one command")
			return true, 2
		}
		if !renderCommandHelp(args[1], stdout) {
			fmt.Fprintf(stderr, "unknown help topic %q\n", args[1])
			return true, 2
		}
		return true, 0
	}
	if !helpFlagBeforeSeparator(args[1:]) {
		return false, 0
	}
	if !renderCommandHelp(args[0], stdout) {
		return false, 0
	}
	return true, 0
}

func helpFlagBeforeSeparator(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func renderCommandHelp(command string, out io.Writer) bool {
	switch command {
	case "daemon":
		daemonUsage(out)
	case "runtime":
		runtimeUsage(out)
	case "doctor":
		runtimeDoctorUsage(out)
	case "run":
		runtimeRunUsage(out)
	case "status", "review", "diff", "approve", "apply", "reject", "release":
		transactionAliasUsage(command, out)
	case "tx":
		transactionUsage(out)
	case "kernel":
		kernelUsage(out)
	case "contracts":
		contractsUsage(out)
	case "action":
		actionUsage(out)
	case "version":
		versionUsage(out)
	case "try", "init", "bootstrap", "compile", "intent", "ir", "plan",
		"artifacts", "spec", "contract", "manifest", "junit", "policy",
		"verify", "gate", "evidence", "approval", "identity":
		// These compatibility and Contracts commands do not yet have isolated
		// usage renderers. Root help still gives a state-independent entry point.
		usage(out)
	default:
		return false
	}
	return true
}

func versionCommand(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintf(stderr, "version: unexpected argument %q\n", args[0])
		versionUsage(stderr)
		return 2
	}
	info := currentCLIVersion()
	if jsonOut {
		data, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}
	commit := info.Commit
	if commit == "" {
		commit = "unknown"
	}
	if info.Dirty {
		commit += "+dirty"
	}
	fmt.Fprintf(stdout, "gatemole %s (commit %s, %s)\n", info.Version, commit, info.GoVersion)
	return 0
}

func currentCLIVersion() cliVersionInfo {
	info := cliVersionInfo{
		Schema:    cliVersionSchema,
		Version:   strings.TrimSpace(buildVersion),
		Commit:    strings.TrimSpace(buildCommit),
		GoVersion: runtime.Version(),
	}
	if info.Version == "" {
		info.Version = "dev"
	}
	if build, ok := debug.ReadBuildInfo(); ok {
		if info.Version == "dev" && build.Main.Version != "" && build.Main.Version != "(devel)" {
			info.Version = build.Main.Version
		}
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = setting.Value
				}
			case "vcs.modified":
				info.Dirty = setting.Value == "true"
			}
		}
	}
	return info
}

func versionUsage(out io.Writer) {
	fmt.Fprintln(out, "usage: gatemole [--json] version")
}

func daemonUsage(out io.Writer) {
	fmt.Fprintln(out, "usage: gatemole [--repo DIR] daemon [options]")
	flags, _ := newDaemonFlagSet("", out)
	flags.PrintDefaults()
}

func runtimeDoctorUsage(out io.Writer) {
	fmt.Fprintln(out, "usage: gatemole [--repo DIR] [--json] doctor [--agent NAME] [--namespace NS] [--require-enforcement-profile development|production] [--runtime-engine ENGINE] [--agent-profiles FILE] [--socket FILE] [--timeout DURATION]")
}

func transactionAliasUsage(command string, out io.Writer) {
	switch command {
	case "status":
		fmt.Fprintln(out, "usage: gatemole [--repo DIR] [--json] status ID [--namespace NS] [--socket FILE]")
	case "review":
		fmt.Fprintln(out, "usage: gatemole [--repo DIR] [--json] review ID [--namespace NS] [--socket FILE]")
	case "diff":
		fmt.Fprintln(out, "usage: gatemole [--repo DIR] [--json] diff ID [--namespace NS] [--socket FILE]")
	case "approve":
		fmt.Fprintln(out, "usage: gatemole [--repo DIR] [--json] approve ID [--namespace NS] --key FILE --key-id ID --approver ID --class CLASS [--decision approve|reject|revise]")
	case "apply":
		fmt.Fprintln(out, "usage: gatemole [--repo DIR] [--json] apply ID [--namespace NS] [--socket FILE] [--actor ID] [--actor-kind KIND]")
	case "reject":
		fmt.Fprintln(out, "usage: gatemole [--repo DIR] [--json] reject ID [--namespace NS] [--socket FILE] [--actor ID] [--actor-kind KIND]")
	case "release":
		fmt.Fprintln(out, "usage: gatemole [--repo DIR] [--json] release ID [--namespace NS] [--socket FILE] [--actor ID] [--actor-kind KIND]")
	}
}

package vouch

import (
	"fmt"
	"io"
)

var contractModuleCommands = map[string]struct{}{
	"try":       {},
	"init":      {},
	"bootstrap": {},
	"compile":   {},
	"intent":    {},
	"ir":        {},
	"plan":      {},
	"artifacts": {},
	"spec":      {},
	"contract":  {},
	"manifest":  {},
	"junit":     {},
	"policy":    {},
	"verify":    {},
	"gate":      {},
	"evidence":  {},
}

// contractsCommand gives the optional release-contract compiler a coherent
// namespace while forwarding to the original commands for compatibility.
// Main remains the single implementation of those mature command paths.
func contractsCommand(
	repo, manifest string,
	jsonOut bool,
	args []string,
	stdout, stderr io.Writer,
) int {
	if len(args) == 0 {
		contractsUsage(stderr)
		return 2
	}
	if _, allowed := contractModuleCommands[args[0]]; !allowed {
		fmt.Fprintf(stderr, "contracts: unknown command %q\n", args[0])
		contractsUsage(stderr)
		return 2
	}
	forwarded := []string{"--repo", repo}
	if manifest != "" {
		forwarded = append(forwarded, "--manifest", manifest)
	}
	if jsonOut {
		forwarded = append(forwarded, "--json")
	}
	forwarded = append(forwarded, args...)
	return Main(forwarded, stdout, stderr)
}

func contractsUsage(out io.Writer) {
	fmt.Fprintln(out, "usage: vouch [--repo DIR] [--json] contracts <command>")
	fmt.Fprintln(out, "  contracts try|init|bootstrap|compile|verify|gate|evidence")
	fmt.Fprintln(out, "  contracts intent|ir|plan|artifacts|spec|contract|manifest|junit|policy")
}

package bootstrap

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

type ownerRule struct {
	Pattern string
	Owners  []string
}

func loadOwners(repo string) []ownerRule {
	var rules []ownerRule
	for _, name := range []string{"CODEOWNERS", ".github/CODEOWNERS", "docs/CODEOWNERS"} {
		data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(name)))
		if err == nil {
			rules = append(rules, parseOwners(string(data))...)
		}
	}
	return rules
}

func parseOwners(data string) []ownerRule {
	var rules []ownerRule
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		rules = append(rules, ownerRule{Pattern: fields[0], Owners: fields[1:]})
	}
	return rules
}

func ownerForPath(rules []ownerRule, relPath string) string {
	for i := len(rules) - 1; i >= 0; i-- {
		if ownerPatternMatches(rules[i].Pattern, relPath) && len(rules[i].Owners) > 0 {
			return strings.TrimPrefix(rules[i].Owners[0], "@")
		}
	}
	return "unowned"
}

func ownerPatternMatches(pattern string, relPath string) bool {
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "/")
	relPath = filepath.ToSlash(relPath)
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(relPath, strings.TrimSuffix(pattern, "**"))
	}
	if strings.HasSuffix(pattern, "/") {
		return strings.HasPrefix(relPath, pattern)
	}
	if strings.ContainsAny(pattern, "*?[") {
		matched, err := path.Match(pattern, relPath)
		if err == nil && matched {
			return true
		}
	}
	return relPath == pattern || strings.HasPrefix(relPath, strings.TrimSuffix(pattern, "/")+"/")
}

package bootstrap

import (
	"os"
	"regexp"
	"strings"
)

var (
	pythonTestPattern = regexp.MustCompile(`(?m)^\s*def\s+(test_[A-Za-z0-9_]+)\s*\(`)
	goTestPattern     = regexp.MustCompile(`(?m)^\s*func\s+(Test[A-Za-z0-9_]+)\s*\(`)
	jsTestPattern     = regexp.MustCompile(`(?m)\b(?:it|test)\s*\(\s*["']([^"']+)["']`)
)

func isTestFile(path string) bool {
	base := slashBase(path)
	return strings.HasPrefix(path, "tests/") ||
		strings.HasSuffix(base, "_test.go") ||
		(strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py")) ||
		strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, ".spec.ts")
}

func testSymbols(absPath string, relPath string) []string {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil
	}
	var matches [][]string
	switch {
	case strings.HasSuffix(relPath, ".py"):
		matches = pythonTestPattern.FindAllStringSubmatch(string(data), -1)
	case strings.HasSuffix(relPath, ".go"):
		matches = goTestPattern.FindAllStringSubmatch(string(data), -1)
	case strings.HasSuffix(relPath, ".ts"):
		matches = jsTestPattern.FindAllStringSubmatch(string(data), -1)
	}
	symbols := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) > 1 && strings.TrimSpace(match[1]) != "" {
			symbols = append(symbols, strings.TrimSpace(match[1]))
		}
	}
	if len(symbols) == 0 && isTestFile(relPath) {
		return []string{strings.TrimSuffix(slashBase(relPath), slashExt(relPath))}
	}
	return unique(symbols)
}

func testLabel(symbol string) string {
	label := symbol
	label = strings.TrimPrefix(label, "test_")
	label = strings.TrimPrefix(label, "Test")
	label = strings.ReplaceAll(label, "_", " ")
	label = splitCamel(label)
	label = strings.ToLower(strings.TrimSpace(label))
	if label == "" {
		return "required test"
	}
	return label
}

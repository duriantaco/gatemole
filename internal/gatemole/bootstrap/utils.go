package bootstrap

import (
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

func sortedKeys[V any](items map[string]V) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func unique(values []string) []string {
	seen := make(map[string]bool, len(values))
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(filepath.ToSlash(value))
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func slug(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	var b strings.Builder
	previousUnderscore := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			previousUnderscore = false
			continue
		}
		if !previousUnderscore {
			b.WriteByte('_')
			previousUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func splitCamel(value string) string {
	var b strings.Builder
	for i, r := range value {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func slashBase(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	return parts[len(parts)-1]
}

func slashExt(path string) string {
	return filepath.Ext(filepath.ToSlash(path))
}

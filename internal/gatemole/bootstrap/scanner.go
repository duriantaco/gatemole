package bootstrap

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

func scan(repo string, aggressive bool) ([]Signal, error) {
	owners := loadOwners(repo)
	var signals []Signal
	err := filepath.WalkDir(repo, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repo, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if entry.IsDir() {
			if skipDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case isTestFile(rel):
			for _, symbol := range testSymbols(path, rel) {
				signals = append(signals, Signal{
					Type:   "test",
					File:   rel,
					Symbol: symbol,
					Risk:   riskFor(rel, symbol),
				})
			}
		case isWorkflowFile(rel):
			signals = append(signals, Signal{Type: "ci", File: rel, Detail: "github workflow", Risk: "low"})
		case isOpenAPIFile(rel):
			signals = append(signals, Signal{Type: "openapi", File: rel, Detail: "api contract", Risk: riskFor(rel, "api")})
		case isCoverageFile(rel):
			signals = append(signals, Signal{Type: "coverage", File: rel, Detail: "coverage xml", Risk: "low"})
		case sourceLike(rel):
			signals = append(signals, Signal{Type: "path", File: rel, Detail: "source path", Risk: riskFor(rel)})
		}
		if owner := ownerForPath(owners, rel); owner != "unowned" {
			signals = append(signals, Signal{Type: "owner", File: rel, Detail: owner, Risk: "low"})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(signals, func(i, j int) bool {
		if signals[i].File == signals[j].File {
			if signals[i].Type == signals[j].Type {
				return signals[i].Symbol < signals[j].Symbol
			}
			return signals[i].Type < signals[j].Type
		}
		return signals[i].File < signals[j].File
	})
	return signals, nil
}

func skipDir(path string) bool {
	switch path {
	case ".git", ".gatemole", ".vouch", "node_modules", "vendor", "dist", "build", "target":
		return true
	default:
		return strings.HasPrefix(path, ".git/") ||
			strings.HasPrefix(path, ".gatemole/") ||
			strings.HasPrefix(path, ".vouch/")
	}
}

func sourceLike(path string) bool {
	if isTestFile(path) {
		return false
	}
	switch filepath.Ext(path) {
	case ".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".rs":
		return true
	default:
		return false
	}
}

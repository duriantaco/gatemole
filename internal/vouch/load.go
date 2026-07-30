package vouch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
)

func LoadJSON[T any](path string) (T, error) {
	var value T
	data, err := os.ReadFile(path)
	if err != nil {
		return value, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("%s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return value, fmt.Errorf("%s: trailing JSON content after top-level value", path)
	}
	return value, nil
}

func DefaultManifest(repo string) string {
	return filepath.Join(repo, ".gatemole", "change-manifest.json")
}

func LoadSpecs(repo string) (map[string]Spec, error) {
	specDir := filepath.Join(repo, ".gatemole", "specs")
	paths, err := filepath.Glob(filepath.Join(specDir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	specs := make(map[string]Spec)
	for _, path := range paths {
		spec, err := LoadJSON[Spec](path)
		if err != nil {
			return nil, err
		}
		if spec.ID != "" {
			if existing, exists := specs[spec.ID]; exists {
				if reflect.DeepEqual(existing, spec) {
					continue
				}
				return nil, fmt.Errorf("duplicate spec id %q in %s", spec.ID, path)
			}
			specs[spec.ID] = spec
		}
	}
	return specs, nil
}

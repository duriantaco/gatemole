package bootstrap

import "testing"

func TestSkipDirProtectsCurrentAndLegacyControlState(t *testing.T) {
	for _, path := range []string{
		".git",
		".git/objects",
		".gatemole",
		".gatemole/runtime.json",
		".vouch",
		".vouch/kernel.db",
	} {
		if !skipDir(path) {
			t.Errorf("skipDir(%q)=false, want true", path)
		}
	}
	if skipDir("internal/kernel") {
		t.Fatal("ordinary source directory was skipped")
	}
}

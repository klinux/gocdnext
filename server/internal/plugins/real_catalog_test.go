package plugins

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestLoadRealMonorepoCatalog smoke-loads the actual `plugins/`
// dir living next to the server module. Any plugin shipping a
// malformed manifest (missing name, name/dir mismatch, invalid
// yaml) trips this test at CI time — operators don't get to
// ship a catalog entry that then rejects apply requests with
// a cryptic "name/dir mismatch" at runtime.
func TestLoadRealMonorepoCatalog(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	// this file → server/internal/plugins/real_catalog_test.go.
	// Monorepo plugins/ sits three levels up (../../../plugins).
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "plugins")

	c := New()
	if err := c.Load(root); err != nil {
		t.Fatalf("monorepo catalog load failed at %s: %v", root, err)
	}
	names := c.Names()
	if len(names) == 0 {
		t.Fatalf("expected at least one plugin, got zero at %s", root)
	}
	for _, n := range names {
		spec, ok := c.Lookup("gocdnext/" + n)
		if !ok {
			t.Errorf("lookup by uses-ref failed: %s", n)
			continue
		}
		if spec.Description == "" {
			t.Errorf("plugin %q has empty description", n)
		}
	}
}

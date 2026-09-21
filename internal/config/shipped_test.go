package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The configs this repository ships have to parse, because unknown keys are
// errors and a stale one makes the file unloadable rather than ignored.
// deploy/test/forgesync.yaml is what `make run` uses, and it carried an
// `oidc:` block for a sign-in that no longer exists: the documented way to
// run the controller from source could not start at all.
//
// This parses rather than loading, so it does not need the secret files a
// real deployment has; unknown keys are caught at exactly this step.
func TestShippedConfigsHaveNoUnknownKeys(t *testing.T) {
	shipped, err := filepath.Glob("../../deploy/*/forgesync*.yaml")
	if err != nil || len(shipped) == 0 {
		t.Fatalf("no shipped configs found: %v", err)
	}
	for _, path := range shipped {
		t.Run(filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			dec := yaml.NewDecoder(bytes.NewReader(raw))
			dec.KnownFields(true)
			if err := dec.Decode(&Config{}); err != nil {
				t.Errorf("%s: %v", path, err)
			}
		})
	}
}

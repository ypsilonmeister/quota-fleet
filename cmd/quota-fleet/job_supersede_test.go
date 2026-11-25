package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobCreateSupersedeKeyFlag(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fleet.db")
	create := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		full := append([]string{"job", "create", "--db", dbPath, "--title", "publish"}, args...)
		if code := run(full, &stdout, &stderr); code != 0 {
			t.Fatalf("job create exit = %d: %s", code, stderr.String())
		}
		return stdout.String()
	}
	if out := create(); strings.Contains(out, "supersede") {
		t.Errorf("unkeyed output mentions supersede: %s", out)
	}
	create("--supersede-key", "pub")
	out := create("--supersede-key", "pub")
	for _, want := range []string{"supersede_key: pub", "superseded: job #2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %s", want, out)
		}
	}
}

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
	create("--supersede-scope", "pr1", "--supersede-key", "pub")
	create("--supersede-scope", "pr2", "--supersede-key", "pub")
	out := create("--supersede-scope", "pr1", "--supersede-key", "pub")
	for _, want := range []string{"supersede_scope: pr1", "supersede_key: pub", "superseded: job #2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %s", want, out)
		}
	}
}

func TestJobCreateRequiresSupersedeScopeWithKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fleet.db")
	for _, flag := range []string{"--supersede-key", "--supersede-scope"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{"job", "create", "--db", dbPath, "--title", "publish", flag, "x"}, &stdout, &stderr)
		if code == 0 || !strings.Contains(stderr.String(), "must be specified together") {
			t.Errorf("%s alone = %d: %s%s", flag, code, stdout.String(), stderr.String())
		}
	}
}

// A keyed draft would cancel runnable work and leave nothing to run.
func TestJobCreateRejectsKeyedDraft(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fleet.db")
	create := func(args ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		full := append([]string{"job", "create", "--db", dbPath, "--title", "publish",
			"--supersede-scope", "pr1", "--supersede-key", "pub"}, args...)
		return run(full, &stdout, &stderr), stdout.String() + stderr.String()
	}
	if code, out := create(); code != 0 {
		t.Fatalf("job create exit = %d: %s", code, out)
	}
	if code, out := create("--draft"); code == 0 || !strings.Contains(out, "draft") {
		t.Fatalf("keyed draft = %d: %s", code, out)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "get", "1", "--db", dbPath}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), "queued") {
		t.Fatalf("job get = %d: %s%s", code, stdout.String(), stderr.String())
	}
}

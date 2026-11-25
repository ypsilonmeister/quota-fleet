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

func TestJobCreateSupersedeKeyWithoutScope(t *testing.T) {
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
	create("--supersede-key", "pub")
	create("--supersede-scope", "pr1", "--supersede-key", "pub")
	out := create("--supersede-key", "pub")
	for _, want := range []string{"supersede_key: pub", "superseded: job #1\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "supersede_scope") || strings.Contains(out, "superseded: job #2") {
		t.Errorf("a key alone reached the scoped job or printed a scope: %s", out)
	}
	for id, want := range map[string]string{"1": "cancelled", "2": "queued", "3": "queued"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"job", "get", id, "--db", dbPath}, &stdout, &stderr); code != 0 ||
			!strings.Contains(stdout.String(), want) {
			t.Errorf("job get %s = %d, want %s: %s%s", id, code, want, stdout.String(), stderr.String())
		}
	}
}

func TestJobCreateRejectsSupersedeScopeWithoutKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fleet.db")
	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "create", "--db", dbPath, "--title", "publish", "--supersede-scope", "pr1"}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "supersede_scope requires supersede_key") {
		t.Errorf("scope alone = %d: %s%s", code, stdout.String(), stderr.String())
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

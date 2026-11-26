package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/quota"
)

func TestCLIUsageValidateAndGeneratorAndAtomicDeployE2E(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "fleet.yaml")
	genOut := filepath.Join(dir, "generated.json")
	destPath := filepath.Join(dir, "deployed-usage.json")

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("Abs repoRoot: %v", err)
	}
	genScript := filepath.Join(repoRoot, "examples", "usage-generator", "generate_sample.py")
	deployScript := filepath.Join(repoRoot, "examples", "usage-generator", "atomic_deploy.sh")

	cfgYAML := `
instance_id: "e2e-usage-phase1"
state_dir: "` + filepath.ToSlash(filepath.Join(dir, "state")) + `"
agents:
  claude: { launch: "claude -p" }
  codex:  { launch: "codex exec" }
pools:
  demo-pool:
    applies_to: [{ agent: claude }]
  demo-alt:
    applies_to: [{ agent: claude }]
  demo-unmeasured:
    unmeasured: true
quota:
  usage_file: "` + filepath.ToSlash(destPath) + `"
  usage_format: "v1"
  stale_after: "45m"
  gate_mode: "dry-run"
`
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("WriteFile fleet.yaml: %v", err)
	}

	// 1. Minimal generator produces valid Schema v1 for arbitrary pools (demo-pool + demo-alt) and warns against enforce.
	helpOut, err := exec.Command("python3", genScript, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("generate_sample.py --help: %v\n%s", err, string(helpOut))
	}
	if !strings.Contains(string(helpOut), "gate_mode: enforce") {
		t.Errorf("generate_sample.py --help missing non-operational enforce warning:\n%s", string(helpOut))
	}
	if out, err := exec.Command("python3", genScript, "--pool", "demo-pool", "--weekly", "80", "--short", "60", "--extra-pool", "demo-alt:70:50", "--out", genOut).CombinedOutput(); err != nil {
		t.Fatalf("generate_sample.py failed: %v\n%s", err, string(out))
	}

	var outBuf, errBuf bytes.Buffer
	code := run([]string{"usage", "validate", "--file", genOut, "--config", cfgPath, "--format", "v1", "--json"}, &outBuf, &errBuf)
	if code != 0 {
		t.Fatalf("usage validate generated exit=%d stdout=%s stderr=%s", code, outBuf.String(), errBuf.String())
	}
	var vr quota.ValidationResult
	if err := json.Unmarshal(outBuf.Bytes(), &vr); err != nil {
		t.Fatalf("unmarshal validation json: %v", err)
	}
	if !vr.OK || !vr.Deployable || vr.Format != "v1" {
		t.Fatalf("unexpected validation result: %+v", vr)
	}

	// 2. Build binary once and test atomic_deploy.sh (flock + --check-deploy --reject-older-than + SHA-256 no-op + mv).
	binPath := filepath.Join(dir, "quota-fleet-bin")
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if bout, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, string(bout))
	}

	now := time.Now().UTC().Truncate(time.Second)
	vNew := filepath.Join(dir, "v_new.json")
	vOld := filepath.Join(dir, "v_old.json")
	vSameDiff := filepath.Join(dir, "v_same_diff.json")
	vPartial := filepath.Join(dir, "v_partial.json")
	vCorrupt := filepath.Join(dir, "v_corrupt.json")

	write := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", p, err)
		}
	}
	tNew := now.Add(-2 * time.Minute).Format(time.RFC3339)
	tOld := now.Add(-10 * time.Minute).Format(time.RFC3339)
	tPart := now.Add(-1 * time.Minute).Format(time.RFC3339)

	write(vNew, `{"schema_version":1,"generated_at":"`+tNew+`","pools":{"demo-pool":{"status":"ok","measured_at":"`+tNew+`","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}},"demo-alt":{"status":"ok","measured_at":"`+tNew+`","weekly":{"remaining_percent":70},"short":{"remaining_percent":50}}}}`)
	write(vOld, `{"schema_version":1,"generated_at":"`+tOld+`","pools":{"demo-pool":{"status":"ok","measured_at":"`+tOld+`","weekly":{"remaining_percent":10},"short":{"remaining_percent":10}}}}`)
	write(vSameDiff, `{"schema_version":1,"generated_at":"`+tNew+`","pools":{"demo-pool":{"status":"ok","measured_at":"`+tNew+`","weekly":{"remaining_percent":5},"short":null}}}`)
	write(vPartial, `{"schema_version":1,"generated_at":"`+tPart+`","pools":{"demo-pool":{"status":"error","error":"probe_timeout","measured_at":"`+tNew+`","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}},"demo-alt":{"status":"ok","measured_at":"`+tPart+`","weekly":{"remaining_percent":95},"short":{"remaining_percent":90}}}}`)
	write(vCorrupt, `{"schema_version":1,"generated_at":"`+tPart+`","pools":{"demo-pool":{"measured_at":"`+tPart+`","weekly":{"remaining_percent":101}}}}`)

	// Initial atomic deploy succeeds.
	if out, err := exec.Command("bash", deployScript, vNew, destPath, binPath).CombinedOutput(); err != nil {
		t.Fatalf("atomic_deploy initial failed: %v\n%s", err, string(out))
	}
	deployedBytes, _ := os.ReadFile(destPath)

	// Re-deploying identical file succeeds (no-op).
	if out, err := exec.Command("bash", deployScript, vNew, destPath, binPath).CombinedOutput(); err != nil {
		t.Fatalf("atomic_deploy identical no-op failed: %v\n%s", err, string(out))
	}

	// Older generation is rejected and existing file is preserved.
	if out, err := exec.Command("bash", deployScript, vOld, destPath, binPath).CombinedOutput(); err == nil {
		t.Fatalf("atomic_deploy older should fail, output:\n%s", string(out))
	}
	afterOld, _ := os.ReadFile(destPath)
	if !bytes.Equal(deployedBytes, afterOld) {
		t.Fatalf("existing dest was overwritten by older generation")
	}

	// Item C: Equal timestamp with different content is rejected and existing file is preserved.
	if out, err := exec.Command("bash", deployScript, vSameDiff, destPath, binPath).CombinedOutput(); err == nil {
		t.Fatalf("atomic_deploy equal-timestamp diff content should fail, output:\n%s", string(out))
	}
	afterDiff, _ := os.ReadFile(destPath)
	if !bytes.Equal(deployedBytes, afterDiff) {
		t.Fatalf("existing dest was overwritten by equal-timestamp conflicting content")
	}

	// Corrupt / schema-invalid file is rejected and existing file is preserved.
	if out, err := exec.Command("bash", deployScript, vCorrupt, destPath, binPath).CombinedOutput(); err == nil {
		t.Fatalf("atomic_deploy corrupt should fail, output:\n%s", string(out))
	}

	// Newer partial failure (status=error + healthy sibling) deploys cleanly via --check-deploy!
	if out, err := exec.Command("bash", deployScript, vPartial, destPath, binPath).CombinedOutput(); err != nil {
		t.Fatalf("atomic_deploy partial failure should succeed: %v\n%s", err, string(out))
	}
	// Normal usage validate on deployed partial file exits 2!
	outBuf.Reset()
	errBuf.Reset()
	if code := run([]string{"usage", "validate", "--config", cfgPath}, &outBuf, &errBuf); code != 2 {
		t.Fatalf("usage validate deployed partial exit=%d, want 2; out=%s", code, outBuf.String())
	}

	// Destination that is an existing directory must fail non-zero and NOT receive a moved temp file (§9.2.2).
	destDir := filepath.Join(dir, "dest-is-directory")
	if err := os.Mkdir(destDir, 0o755); err != nil {
		t.Fatalf("Mkdir destDir: %v", err)
	}
	if out, err := exec.Command("bash", deployScript, vNew, destDir, binPath).CombinedOutput(); err == nil {
		t.Fatalf("atomic_deploy to existing directory should fail non-zero, output:\n%s", string(out))
	}
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("ReadDir destDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("atomic_deploy moved files into destination directory: %v", entries)
	}

	// Unsanitized collector error containing newline / raw upstream text / single-line dummy credential
	// is sanitized in CLI text & JSON outputs while preserving collector_error and exit codes.
	const dummySingleLineCred = "sk-proj-DummySecretToken1234567890"
	vRawErr := filepath.Join(dir, "v_raw_err.json")
	write(vRawErr, `{"schema_version":1,"generated_at":"`+tPart+`","pools":{"demo-pool":{"status":"error","error":"probe_timeout\nREVIEW_TEST_RAW_UPSTREAM_LINE","measured_at":"`+tNew+`","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}},"demo-alt":{"status":"error","error":"`+dummySingleLineCred+`","measured_at":"`+tNew+`","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}}}}`)

	outBuf.Reset()
	errBuf.Reset()
	if code := run([]string{"usage", "validate", "--file", vRawErr, "--format", "v1"}, &outBuf, &errBuf); code != 2 {
		t.Fatalf("usage validate unsanitized collector error exit=%d, want 2; out=%s err=%s", code, outBuf.String(), errBuf.String())
	}
	valOut := outBuf.String()
	if strings.Contains(valOut, "REVIEW_TEST_RAW_UPSTREAM_LINE") || strings.Contains(valOut, dummySingleLineCred) {
		t.Errorf("usage validate leaked raw collector error:\n%s", valOut)
	}
	if !strings.Contains(valOut, "collector_error") || !strings.Contains(valOut, quota.RedactedCollectorError) {
		t.Errorf("usage validate missing collector_error/%s:\n%s", quota.RedactedCollectorError, valOut)
	}

	outBuf.Reset()
	errBuf.Reset()
	if code := run([]string{"usage", "validate", "--file", vRawErr, "--format", "v1", "--json"}, &outBuf, &errBuf); code != 2 {
		t.Fatalf("usage validate --json single-line credential exit=%d, want 2; out=%s err=%s", code, outBuf.String(), errBuf.String())
	}
	valJSON := outBuf.String()
	if strings.Contains(valJSON, "REVIEW_TEST_RAW_UPSTREAM_LINE") || strings.Contains(valJSON, dummySingleLineCred) {
		t.Errorf("usage validate --json leaked raw collector error:\n%s", valJSON)
	}
	if !strings.Contains(valJSON, `"code": "collector_error"`) || !strings.Contains(valJSON, `"pool_error": "`+quota.RedactedCollectorError+`"`) {
		t.Errorf("usage validate --json missing sanitized collector_error fields:\n%s", valJSON)
	}

	outBuf.Reset()
	errBuf.Reset()
	if code := run([]string{"usage", "validate", "--file", vRawErr, "--format", "v1", "--check-deploy"}, &outBuf, &errBuf); code != 0 {
		t.Fatalf("usage validate --check-deploy unsanitized collector error exit=%d, want 0; out=%s err=%s", code, outBuf.String(), errBuf.String())
	}

	outBuf.Reset()
	errBuf.Reset()
	if code := run([]string{"gate", "check", "--config", cfgPath, "--usage-file", vRawErr, "--format", "v1", "--pool", "demo-pool"}, &outBuf, &errBuf); code != 0 {
		t.Fatalf("gate check unsanitized collector error exit=%d, want 0; out=%s err=%s", code, outBuf.String(), errBuf.String())
	}
	gateOut := outBuf.String()
	if strings.Contains(gateOut, "REVIEW_TEST_RAW_UPSTREAM_LINE") {
		t.Errorf("gate check leaked raw collector error:\n%s", gateOut)
	}
	if !strings.Contains(gateOut, "reason:        collector_error") || !strings.Contains(gateOut, "pool_error:    "+quota.RedactedCollectorError) {
		t.Errorf("gate check missing sanitized collector_error fields:\n%s", gateOut)
	}

	// Single-line dummy credential in gate check --json also redacts pool_error/detail/snapshot while preserving collector_error and exit 0.
	outBuf.Reset()
	errBuf.Reset()
	if code := run([]string{"gate", "check", "--config", cfgPath, "--usage-file", vRawErr, "--format", "v1", "--pool", "demo-alt", "--json"}, &outBuf, &errBuf); code != 0 {
		t.Fatalf("gate check --json single-line credential exit=%d, want 0; out=%s err=%s", code, outBuf.String(), errBuf.String())
	}
	gateJSON := outBuf.String()
	if strings.Contains(gateJSON, dummySingleLineCred) {
		t.Errorf("gate check --json leaked single-line dummy credential:\n%s", gateJSON)
	}
	if !strings.Contains(gateJSON, `"reason": "collector_error"`) || !strings.Contains(gateJSON, `"pool_error": "`+quota.RedactedCollectorError+`"`) {
		t.Errorf("gate check --json missing sanitized collector_error fields:\n%s", gateJSON)
	}
}

func TestCLIGateCheckExitCodeAndItemEFlags(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "fleet.yaml")
	legacyPath := filepath.Join(dir, "legacy.json")
	v1Path := filepath.Join(dir, "shadow-v1.json")

	fixedNow := "2026-11-26T21:00:00Z"
	cfgYAML := `
instance_id: "gate-check-e"
state_dir: "` + filepath.ToSlash(filepath.Join(dir, "state")) + `"
pools:
  claude: {}
  codex: {}
  agy-argon:
    unmeasured: true
quota:
  usage_file: "` + filepath.ToSlash(legacyPath) + `"
  usage_format: "legacy"
  stale_after: "45m"
  gate_mode: "enforce"
`
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("WriteFile cfg: %v", err)
	}
	if err := os.WriteFile(v1Path, []byte(`{
		"schema_version": 1,
		"generated_at": "2026-11-26T20:50:00Z",
		"pools": {
			"claude": {"status":"ok","measured_at":"2026-11-26T20:49:50Z","weekly":{"remaining_percent":80,"reset_at":"2026-11-25T00:00:00Z"},"short":{"remaining_percent":60}},
			"codex":  {"status":"ok","measured_at":"2026-11-26T20:49:50Z","weekly":{"remaining_percent":70,"reset_at":"2026-12-01T00:00:00Z"},"short":{"remaining_percent":50}}
		}
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile v1: %v", err)
	}

	// 1. Decision 9: missing usage file exits 1 in both text and --json mode!
	var out, errBuf bytes.Buffer
	if code := run([]string{"gate", "check", "--config", cfgPath, "--pool", "claude"}, &out, &errBuf); code != 1 {
		t.Fatalf("gate check missing file text exit=%d, want 1", code)
	}
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"gate", "check", "--config", cfgPath, "--pool", "claude", "--json"}, &out, &errBuf); code != 1 {
		t.Fatalf("gate check missing file json exit=%d, want 1", code)
	}
	if !strings.Contains(out.String(), `"decision": "unknown"`) || !strings.Contains(out.String(), `"error":`) {
		t.Errorf("gate check --json missing expected fields:\n%s", out.String())
	}

	// 2. Item E: --format v1 overrides legacy config, and --now replays at historical timestamp (choosing codex over past-reset claude).
	out.Reset()
	errBuf.Reset()
	code := run([]string{
		"gate", "check",
		"--config", cfgPath,
		"--usage-file", v1Path,
		"--format", "v1",
		"--pool", "claude",
		"--alt-pools", "codex",
		"--now", fixedNow,
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("gate check side-by-side v1 exit=%d stderr=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "format:        v1") || !strings.Contains(out.String(), "selected_pool: codex") {
		t.Errorf("unexpected side-by-side v1 output:\n%s", out.String())
	}

	// 3. Item E: empty --pool with --alt-pools agy-argon reproduces unmeasured-alt-only bypass!
	out.Reset()
	errBuf.Reset()
	code = run([]string{
		"gate", "check",
		"--config", cfgPath,
		"--usage-file", v1Path,
		"--format", "v1",
		"--alt-pools", "agy-argon",
		"--now", fixedNow,
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("gate check alt-pools-only exit=%d stderr=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "selected_pool: agy-argon") || !strings.Contains(out.String(), "bypassed:      true") {
		t.Errorf("unexpected alt-pools-only output:\n%s", out.String())
	}

	// 3b. Explicit --config / -c to a nonexistent file MUST fail with exit 1 even when --usage-file is valid.
	missingCfg := filepath.Join(dir, "does-not-exist.yaml")
	for _, flagName := range []string{"--config", "-c"} {
		out.Reset()
		errBuf.Reset()
		code = run([]string{
			"gate", "check",
			flagName, missingCfg,
			"--usage-file", v1Path,
			"--format", "v1",
			"--pool", "claude",
			"--now", fixedNow,
			"--json",
		}, &out, &errBuf)
		if code != 1 {
			t.Fatalf("gate check explicit %s nonexistent exit=%d, want 1; stdout=%s stderr=%s", flagName, code, out.String(), errBuf.String())
		}
		if !strings.Contains(errBuf.String(), "gate check failed") {
			t.Errorf("gate check explicit %s nonexistent stderr=%q, want error message", flagName, errBuf.String())
		}
	}

	// 3c. Standalone gate check WITHOUT --config/-c succeeds when default fleet.yaml does not exist.
	t.Setenv("QUOTA_FLEET_CONFIG", filepath.Join(dir, "default-missing-fleet.yaml"))
	out.Reset()
	errBuf.Reset()
	code = run([]string{
		"gate", "check",
		"--usage-file", v1Path,
		"--format", "v1",
		"--pool", "claude",
		"--now", fixedNow,
		"--json",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("standalone gate check without --config exit=%d, want 0; stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), `"decision": "would_pass"`) || !strings.Contains(out.String(), `"selected_pool": "claude"`) {
		t.Errorf("unexpected standalone gate check output:\n%s", out.String())
	}

	// 4. Init template includes commented Quota Gate snippet & next-step hint (§8.1).
	initCfg := filepath.Join(dir, "init-fleet.yaml")
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"init", "--config", initCfg}, &out, &errBuf); code != 0 {
		t.Fatalf("init exit=%d stderr=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "docs/23-usage-snapshot.md") {
		t.Errorf("init stdout missing quota_gate hint:\n%s", out.String())
	}
	rawInit, _ := os.ReadFile(initCfg)
	for _, want := range []string{"docs/23-usage-snapshot.md", `usage_format: "v1"`, "short_window: false", `gate_mode: "dry-run"`} {
		if !strings.Contains(string(rawInit), want) {
			t.Errorf("init fleet.yaml missing %q", want)
		}
	}
}

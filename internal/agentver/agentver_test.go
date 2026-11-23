package agentver

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
	_ "modernc.org/sqlite"
)

type fakeFileAccess struct {
	runFn func(ctx context.Context, command string) (adapter.ExecResult, error)
}

func (f *fakeFileAccess) Stat(ctx context.Context, p string) (adapter.FileStat, error) {
	return adapter.FileStat{}, nil
}

func (f *fakeFileAccess) ReadFile(ctx context.Context, p string) ([]byte, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeFileAccess) RunCommand(ctx context.Context, command string) (adapter.ExecResult, error) {
	return f.runFn(ctx, command)
}

type fakeHostProvider struct {
	hosts map[string]adapter.FileAccess
	errs  map[string]error
}

func (p *fakeHostProvider) FileAccessForHost(hostName string) (adapter.FileAccess, error) {
	if err, ok := p.errs[hostName]; ok {
		return nil, err
	}
	fa, ok := p.hosts[hostName]
	if !ok {
		return nil, errors.New("unknown host: " + hostName)
	}
	return fa, nil
}

func TestNormalizeAndCompareSemver(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"2.1.76 (Claude Code)\n", "2.1.76"},
		{"codex-cli 0.169.0", "0.169.0"},
		{"agy v0.4.2-beta.1+build7", "0.4.2-beta.1+build7"},
		{"nightly-20261124\nextra line", "nightly-20261124"},
		{"   \n\n  ", ""},
	}
	for _, tc := range cases {
		if got := NormalizeVersion(tc.raw); got != tc.want {
			t.Errorf("NormalizeVersion(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	v1, ok := ParseSemver("0.169.0")
	if !ok {
		t.Fatal("expected 0.169.0 to parse as semver")
	}
	v2, ok := ParseSemver("0.171.0")
	if !ok {
		t.Fatal("expected 0.171.0 to parse as semver")
	}
	if CompareSemver(v1, v2) >= 0 {
		t.Errorf("expected 0.169.0 < 0.171.0")
	}
	pre, ok := ParseSemver("1.0.0-beta.1")
	if !ok {
		t.Fatal("expected 1.0.0-beta.1 to parse")
	}
	rel, ok := ParseSemver("1.0.0")
	if !ok {
		t.Fatal("expected 1.0.0 to parse")
	}
	if CompareSemver(pre, rel) >= 0 {
		t.Errorf("expected 1.0.0-beta.1 < 1.0.0")
	}
	if _, ok := ParseSemver("nightly-20261124"); ok {
		t.Errorf("expected non-semver string to fail ParseSemver")
	}
}

func TestBuildMatrix_DriftAndPinExclusionAndNonSemver(t *testing.T) {
	now := time.Date(2026, 11, 24, 7, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		InstanceID: "test",
		Hosts: map[string]config.HostConfig{
			"ubuntu":    {},
			"ubuntu-vm": {Agents: map[string]config.HostAgentOverride{"claude": {ManualUpdate: true}}},
			"home-win":  {OS: "windows", Agents: map[string]config.HostAgentOverride{"codex": {Pin: "0.169.0"}}},
		},
		Agents: map[string]config.AgentConfig{
			"claude": {},
			"codex":  {},
			"agy":    {},
		},
	}

	rows := []*store.AgentVersion{
		// claude: ubuntu 2.1.76, ubuntu-vm 2.1.70 (manual_update), home-win 2.1.76 -> DRIFT!
		{Host: "ubuntu", Agent: "claude", Version: "2.1.76", Status: store.AgentVersionStatusOK, CheckedAt: now},
		{Host: "ubuntu-vm", Agent: "claude", Version: "2.1.70", Status: store.AgentVersionStatusOK, CheckedAt: now},
		{Host: "home-win", Agent: "claude", Version: "2.1.76", Status: store.AgentVersionStatusOK, CheckedAt: now},
		// codex: ubuntu 0.171.0, ubuntu-vm 0.171.0, home-win 0.169.0 (PINNED) -> DRIFT detected, home-win excluded from lag!
		{Host: "ubuntu", Agent: "codex", Version: "0.171.0", Status: store.AgentVersionStatusOK, CheckedAt: now},
		{Host: "ubuntu-vm", Agent: "codex", Version: "0.171.0", Status: store.AgentVersionStatusOK, CheckedAt: now},
		{Host: "home-win", Agent: "codex", Version: "0.169.0", Status: store.AgentVersionStatusOK, CheckedAt: now},
		// agy: non-semver versions differ -> DRIFT, Comparable=false, no lagging hosts asserted!
		{Host: "ubuntu", Agent: "agy", Version: "nightly-20261124", Status: store.AgentVersionStatusOK, CheckedAt: now},
		{Host: "ubuntu-vm", Agent: "agy", Version: "nightly-20261123", Status: store.AgentVersionStatusOK, CheckedAt: now},
		{Host: "home-win", Agent: "agy", Status: store.AgentVersionStatusNotFound, ErrorDetail: "not found", CheckedAt: now},
	}

	m := BuildMatrix(cfg, rows)
	if !m.HasDrift {
		t.Fatal("expected HasDrift=true")
	}

	sumByAgent := make(map[string]AgentSummary)
	for _, s := range m.Summaries {
		sumByAgent[s.Agent] = s
	}

	// Verify claude drift + manual_update.
	claudeSum := sumByAgent["claude"]
	if !claudeSum.Drifted || !claudeSum.Comparable || claudeSum.MaxVersion != "2.1.76" {
		t.Errorf("unexpected claude summary: %+v", claudeSum)
	}
	if len(claudeSum.LaggingHosts) != 1 || claudeSum.LaggingHosts[0] != "ubuntu-vm" {
		t.Errorf("claude lagging_hosts = %v, want [ubuntu-vm]", claudeSum.LaggingHosts)
	}
	if len(claudeSum.ManualUpdateHosts) != 1 || claudeSum.ManualUpdateHosts[0] != "ubuntu-vm" {
		t.Errorf("claude manual_update_hosts = %v, want [ubuntu-vm]", claudeSum.ManualUpdateHosts)
	}

	// Verify codex pin rule (docs/08 §3): differing pinned host STILL triggers drift,
	// while pin excludes home-win ONLY from lagging_hosts / Behind.
	codexSum := sumByAgent["codex"]
	if !codexSum.Drifted {
		t.Errorf("expected codex Drifted=true even when only differing host is pinned, got %+v", codexSum)
	}
	if !codexSum.Comparable || codexSum.MaxVersion != "0.171.0" {
		t.Errorf("expected codex Comparable=true MaxVersion=0.171.0, got %+v", codexSum)
	}
	if len(codexSum.Versions) != 2 || codexSum.Versions[0] != "0.169.0" || codexSum.Versions[1] != "0.171.0" {
		t.Errorf("expected codex versions=[0.169.0 0.171.0], got %v", codexSum.Versions)
	}
	if len(codexSum.LaggingHosts) != 0 {
		t.Errorf("expected codex lagging_hosts empty (pinned host excluded from lag), got %v", codexSum.LaggingHosts)
	}
	if len(codexSum.PinnedHosts) != 1 || codexSum.PinnedHosts[0] != "home-win" {
		t.Errorf("expected codex pinned_hosts=[home-win], got %v", codexSum.PinnedHosts)
	}
	for _, c := range m.Entries {
		if c.Host == "home-win" && c.Agent == "codex" {
			if !c.Pinned || c.Pin != "0.169.0" || c.Behind {
				t.Errorf("home-win/codex cell = %+v, want Pinned=true Pin=0.169.0 Behind=false", c)
			}
		}
	}

	// Verify agy non-semver: drifted=true, comparable=false, max_version="", lagging=[]
	agySum := sumByAgent["agy"]
	if !agySum.Drifted || agySum.Comparable || agySum.MaxVersion != "" {
		t.Errorf("unexpected agy non-semver summary: %+v", agySum)
	}
	if len(agySum.LaggingHosts) != 0 {
		t.Errorf("non-semver agent must not assert lagging hosts, got %v", agySum.LaggingHosts)
	}
	if len(agySum.FailedHosts) != 1 || agySum.FailedHosts[0] != "home-win" {
		t.Errorf("agy failed_hosts = %v, want [home-win]", agySum.FailedHosts)
	}
}

func TestChecker_DriftLifecyclePinExclusionAndProbeFailures(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:", store.Options{InstanceID: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		InstanceID: "test",
		Hosts: map[string]config.HostConfig{
			"ubuntu":    {Transport: "local"},
			"ubuntu-vm": {Transport: "ssh", SSH: "ubuntu-vm", Agents: map[string]config.HostAgentOverride{"claude": {ManualUpdate: true}}},
			"home-win":  {Transport: "ssh", SSH: "home-win", OS: "windows", Agents: map[string]config.HostAgentOverride{"codex": {Pin: "0.169.0"}}},
		},
		Agents: map[string]config.AgentConfig{
			"claude": {},
			"codex":  {},
			"agy":    {VersionArgs: config.StringList{"-v"}},
		},
	}

	// Mutable probe outputs per (host, command substring).
	outputs := map[string]func(cmd string) (adapter.ExecResult, error){
		"ubuntu": func(cmd string) (adapter.ExecResult, error) {
			switch {
			case strings.Contains(cmd, "claude"):
				return adapter.ExecResult{ExitCode: 0, Output: "2.1.76 (Claude Code)\n"}, nil
			case strings.Contains(cmd, "codex"):
				return adapter.ExecResult{ExitCode: 0, Output: "codex-cli 0.171.0\n"}, nil
			case strings.Contains(cmd, "agy") && strings.Contains(cmd, "-v"):
				return adapter.ExecResult{ExitCode: 0, Output: "agy 0.5.0\n"}, nil
			default:
				return adapter.ExecResult{ExitCode: 127, Output: adapter.VersionProbeNotFoundMarker + " unknown\n"}, nil
			}
		},
		"ubuntu-vm": func(cmd string) (adapter.ExecResult, error) {
			switch {
			case strings.Contains(cmd, "claude"):
				return adapter.ExecResult{ExitCode: 0, Output: "2.1.76 (Claude Code)\n"}, nil
			case strings.Contains(cmd, "codex"):
				return adapter.ExecResult{ExitCode: 0, Output: "codex-cli 0.171.0\n"}, nil
			default:
				return adapter.ExecResult{ExitCode: 0, Output: "agy 0.5.0\n"}, nil
			}
		},
		"home-win": func(cmd string) (adapter.ExecResult, error) {
			switch {
			case strings.Contains(cmd, "claude"):
				return adapter.ExecResult{ExitCode: 0, Output: "2.1.76 (Claude Code)\n"}, nil
			case strings.Contains(cmd, "codex"):
				return adapter.ExecResult{ExitCode: 0, Output: "codex-cli 0.169.0\n"}, nil
			default:
				return adapter.ExecResult{ExitCode: 0, Output: "agy 0.5.0\n"}, nil
			}
		},
	}

	provider := &fakeHostProvider{
		hosts: map[string]adapter.FileAccess{
			"ubuntu":    &fakeFileAccess{runFn: func(ctx context.Context, cmd string) (adapter.ExecResult, error) { return outputs["ubuntu"](cmd) }},
			"ubuntu-vm": &fakeFileAccess{runFn: func(ctx context.Context, cmd string) (adapter.ExecResult, error) { return outputs["ubuntu-vm"](cmd) }},
			"home-win":  &fakeFileAccess{runFn: func(ctx context.Context, cmd string) (adapter.ExecResult, error) { return outputs["home-win"](cmd) }},
		},
	}

	now := time.Date(2026, 11, 24, 7, 0, 0, 0, time.UTC)
	var hooked []*store.AgentVersionEvent
	checker, err := NewChecker(CheckerOptions{
		Store:    st,
		Config:   cfg,
		Provider: provider,
		Now:      func() time.Time { return now },
		OnEvent: func(ctx context.Context, ev *store.AgentVersionEvent) {
			hooked = append(hooked, ev)
		},
	})
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}

	// Pass 1: claude & agy aligned; home-win/codex is pinned at 0.169.0 while others are 0.171.0
	// -> codex MUST be detected as drifted and record drift_started, with no lagging hosts.
	res1, err := checker.Check(ctx)
	if err != nil {
		t.Fatalf("Check pass 1: %v", err)
	}
	if !res1.Matrix.HasDrift {
		t.Fatalf("pass 1: expected HasDrift=true because codex has 0.169.0 (pinned) vs 0.171.0, got summaries=%+v", res1.Matrix.Summaries)
	}
	if len(res1.Events) != 1 || res1.Events[0].EventType != store.AgentEventDriftStarted || res1.Events[0].Agent != "codex" || res1.Events[0].Host != "" {
		t.Fatalf("pass 1: expected 1 drift_started event for codex with empty lagging host, got %+v", res1.Events)
	}

	// Pass 2: introduce drift on ubuntu-vm/claude (2.1.76 -> 2.1.70), and simulate not_found + error on agy.
	now = now.Add(time.Minute)
	outputs["ubuntu-vm"] = func(cmd string) (adapter.ExecResult, error) {
		switch {
		case strings.Contains(cmd, "claude"):
			return adapter.ExecResult{ExitCode: 0, Output: "2.1.70 (Claude Code)\n"}, nil
		case strings.Contains(cmd, "codex"):
			return adapter.ExecResult{ExitCode: 0, Output: "codex-cli 0.171.0\n"}, nil
		default:
			return adapter.ExecResult{ExitCode: 127, Output: adapter.VersionProbeNotFoundMarker + " agy\n"}, nil
		}
	}
	outputs["home-win"] = func(cmd string) (adapter.ExecResult, error) {
		switch {
		case strings.Contains(cmd, "claude"):
			return adapter.ExecResult{ExitCode: 0, Output: "2.1.76 (Claude Code)\n"}, nil
		case strings.Contains(cmd, "codex"):
			return adapter.ExecResult{ExitCode: 0, Output: "codex-cli 0.169.0\n"}, nil
		default:
			return adapter.ExecResult{}, errors.New("ssh connection refused")
		}
	}

	res2, err := checker.Check(ctx)
	if err != nil {
		t.Fatalf("Check pass 2: %v", err)
	}
	if !res2.Matrix.HasDrift {
		t.Fatal("pass 2: expected HasDrift=true")
	}

	// Verify failure statuses are distinguished (not_found vs error) and never version "0".
	for _, c := range res2.Matrix.Entries {
		if c.Host == "ubuntu-vm" && c.Agent == "agy" {
			if c.Status != store.AgentVersionStatusNotFound || c.Version != "" {
				t.Errorf("ubuntu-vm/agy = %+v, want status=not_found version=''", c)
			}
		}
		if c.Host == "home-win" && c.Agent == "agy" {
			if c.Status != store.AgentVersionStatusError || c.Version != "" || !strings.Contains(c.ErrorDetail, "connection refused") {
				t.Errorf("home-win/agy = %+v, want status=error version='' detail='connection refused'", c)
			}
		}
	}

	// Verify events: version_changed, drift_started (with manual_update=true), and 2 probe_failed.
	var sawChanged, sawDriftStarted bool
	var probeFailedCount int
	for _, ev := range res2.Events {
		switch ev.EventType {
		case store.AgentEventVersionChanged:
			if ev.Agent == "claude" && ev.Host == "ubuntu-vm" && ev.OldVersion == "2.1.76" && ev.NewVersion == "2.1.70" {
				sawChanged = true
			}
		case store.AgentEventDriftStarted:
			if ev.Agent == "claude" {
				sawDriftStarted = true
				if !ev.ManualUpdate {
					t.Errorf("expected drift_started for claude to have ManualUpdate=true")
				}
				if !strings.Contains(ev.Detail, "手作業") {
					t.Errorf("expected drift_started detail to mention manual update, got %q", ev.Detail)
				}
			}
		case store.AgentEventProbeFailed:
			probeFailedCount++
		}
	}
	if !sawChanged {
		t.Errorf("expected version_changed event for ubuntu-vm/claude, got %+v", res2.Events)
	}
	if !sawDriftStarted {
		t.Errorf("expected drift_started event for claude, got %+v", res2.Events)
	}
	if probeFailedCount != 2 {
		t.Errorf("expected 2 probe_failed events (not_found + error), got %d", probeFailedCount)
	}

	// Pass 3: revert ubuntu-vm/claude back to 2.1.76 and recover agy -> drift_resolved + probe recovery!
	now = now.Add(time.Minute)
	outputs["ubuntu-vm"] = func(cmd string) (adapter.ExecResult, error) {
		switch {
		case strings.Contains(cmd, "claude"):
			return adapter.ExecResult{ExitCode: 0, Output: "2.1.76 (Claude Code)\n"}, nil
		case strings.Contains(cmd, "codex"):
			return adapter.ExecResult{ExitCode: 0, Output: "codex-cli 0.171.0\n"}, nil
		default:
			return adapter.ExecResult{ExitCode: 0, Output: "agy 0.5.0\n"}, nil
		}
	}
	outputs["home-win"] = func(cmd string) (adapter.ExecResult, error) {
		switch {
		case strings.Contains(cmd, "claude"):
			return adapter.ExecResult{ExitCode: 0, Output: "2.1.76 (Claude Code)\n"}, nil
		case strings.Contains(cmd, "codex"):
			return adapter.ExecResult{ExitCode: 0, Output: "codex-cli 0.169.0\n"}, nil
		default:
			return adapter.ExecResult{ExitCode: 0, Output: "agy 0.5.0\n"}, nil
		}
	}

	res3, err := checker.Check(ctx)
	if err != nil {
		t.Fatalf("Check pass 3: %v", err)
	}
	// claude drift is resolved, while codex remains drifted (pinned 0.169.0 vs 0.171.0).
	for _, s := range res3.Matrix.Summaries {
		if s.Agent == "claude" && s.Drifted {
			t.Fatalf("pass 3: expected claude Drifted=false after revert, got %+v", s)
		}
		if s.Agent == "codex" && (!s.Drifted || len(s.LaggingHosts) != 0) {
			t.Fatalf("pass 3: expected codex Drifted=true with 0 lagging hosts, got %+v", s)
		}
	}

	var sawDriftResolved bool
	var sawRecovery int
	for _, ev := range res3.Events {
		if ev.EventType == store.AgentEventDriftResolved && ev.Agent == "claude" {
			sawDriftResolved = true
		}
		if ev.EventType == store.AgentEventProbeFailed && strings.Contains(ev.Detail, "recovered") {
			sawRecovery++
		}
	}
	if !sawDriftResolved {
		t.Errorf("expected drift_resolved event for claude, got %+v", res3.Events)
	}
	if sawRecovery != 2 {
		t.Errorf("expected 2 probe recovery events, got %d", sawRecovery)
	}
	if len(hooked) != len(res1.Events)+len(res2.Events)+len(res3.Events) {
		t.Errorf("expected OnEvent hook called %d times, got %d", len(res1.Events)+len(res2.Events)+len(res3.Events), len(hooked))
	}
}

func TestProbeClassification_DistinguishesNotFoundFromRuntimeErrors(t *testing.T) {
	now := time.Date(2026, 11, 24, 7, 30, 0, 0, time.UTC)

	// 1. Direct classifyProbeResult cases.
	cases := []struct {
		name       string
		targetExe  string
		res        adapter.ExecResult
		runErr     error
		wantStatus store.AgentVersionStatus
	}{
		{
			name:       "pre-check not found marker",
			targetExe:  "agy",
			res:        adapter.ExecResult{ExitCode: 127, Output: adapter.VersionProbeNotFoundMarker + " agy\n"},
			wantStatus: store.AgentVersionStatusNotFound,
		},
		{
			name:       "config missing file error must be error not not_found",
			targetExe:  "claude",
			res:        adapter.ExecResult{ExitCode: 1, Output: "config: no such file or directory"},
			wantStatus: store.AgentVersionStatusError,
		},
		{
			name:       "bare exit 127 without marker or target shell error must be error",
			targetExe:  "claude",
			res:        adapter.ExecResult{ExitCode: 127, Output: ""},
			wantStatus: store.AgentVersionStatusError,
		},
		{
			name:       "wrapper missing child dependency exit 127 must be error",
			targetExe:  "/usr/local/bin/claude-wrapper",
			res:        adapter.ExecResult{ExitCode: 127, Output: "sh: 1: missing-dep: not found"},
			wantStatus: store.AgentVersionStatusError,
		},
		{
			name:       "bare exit 9009 without target message must be error",
			targetExe:  "codex",
			res:        adapter.ExecResult{ExitCode: 9009, Output: "'helper' is not recognized as an internal or external command"},
			wantStatus: store.AgentVersionStatusError,
		},
		{
			name:       "sh not found output without pre-check marker must be error",
			targetExe:  "agy",
			res:        adapter.ExecResult{ExitCode: 127, Output: "sh: 1: agy: not found"},
			wantStatus: store.AgentVersionStatusError,
		},
		{
			name:       "command output naming itself command not found with exit 1 must be error",
			targetExe:  "agent",
			res:        adapter.ExecResult{ExitCode: 1, Output: "agent: command not found"},
			wantStatus: store.AgentVersionStatusError,
		},
		{
			name:       "bash command not found without pre-check marker must be error",
			targetExe:  "codex",
			res:        adapter.ExecResult{ExitCode: 127, Output: "bash: line 1: codex: command not found"},
			wantStatus: store.AgentVersionStatusError,
		},
		{
			name:       "powershell not recognized without pre-check marker must be error",
			targetExe:  "codex",
			res:        adapter.ExecResult{ExitCode: 1, Output: "codex : The term 'codex' is not recognized as the name of a cmdlet, function, script file, or operable program."},
			wantStatus: store.AgentVersionStatusError,
		},
		{
			name:       "powershell not recognized naming different command must be error",
			targetExe:  "codex",
			res:        adapter.ExecResult{ExitCode: 1, Output: "helper : The term 'helper' is not recognized as the name of a cmdlet, function, script file, or operable program."},
			wantStatus: store.AgentVersionStatusError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyProbeResult("h1", "agent", tc.targetExe, tc.res, tc.runErr, now)
			if got.Status != tc.wantStatus {
				t.Errorf("classifyProbeResult() status = %q (detail=%q), want %q", got.Status, got.ErrorDetail, tc.wantStatus)
			}
			if got.Version != "" {
				t.Errorf("failed probe must have empty version, got %q", got.Version)
			}
		})
	}

	// 2. End-to-end execution via LocalFileAccess + BuildVersionCommand.
	dir := t.TempDir()
	pathDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(pathDir, 0o755); err != nil {
		t.Fatalf("mkdir pathDir: %v", err)
	}
	cfgFailBin := filepath.Join(dir, "cfg-fail")
	if err := os.WriteFile(cfgFailBin, []byte("#!/bin/sh\necho 'config: no such file or directory' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write cfg-fail: %v", err)
	}
	wrapperMissingDepBin := filepath.Join(dir, "wrapper-missing-dep")
	if err := os.WriteFile(wrapperMissingDepBin, []byte("#!/bin/sh\nmissing-inner-dep-xyz-999 --version\n"), 0o755); err != nil {
		t.Fatalf("write wrapper-missing-dep: %v", err)
	}
	nonExecBin := filepath.Join(dir, "non-exec")
	if err := os.WriteFile(nonExecBin, []byte("#!/bin/sh\necho 1.0.0\n"), 0o644); err != nil {
		t.Fatalf("write non-exec: %v", err)
	}
	// Repro 1: executable exists and prints "<name>: command not found" on stderr with exit 1.
	selfNotFoundErrBin := filepath.Join(pathDir, "agent-self-err")
	if err := os.WriteFile(selfNotFoundErrBin, []byte("#!/bin/sh\necho 'agent-self-err: command not found' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write agent-self-err: %v", err)
	}
	// Repro 2: executable script exists at absolute path, but its shebang interpreter is missing.
	badShebangBin := filepath.Join(dir, "bad-shebang")
	if err := os.WriteFile(badShebangBin, []byte("#!/nonexistent/qf-missing-interpreter-xyz\necho 1.0.0\n"), 0o755); err != nil {
		t.Fatalf("write bad-shebang: %v", err)
	}
	// Repro 3: file exists on PATH by bare name, but lacks the execute bit.
	nonExecOnPathBin := filepath.Join(pathDir, "non-exec-on-path")
	if err := os.WriteFile(nonExecOnPathBin, []byte("#!/bin/sh\necho 1.0.0\n"), 0o644); err != nil {
		t.Fatalf("write non-exec-on-path: %v", err)
	}
	missingPathBin := filepath.Join(dir, "does-not-exist-bin")

	st, err := store.Open(context.Background(), ":memory:", store.Options{InstanceID: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	pathEnv := map[string]string{"PATH": pathDir + ":/usr/bin:/bin"}
	cfg := &config.Config{
		InstanceID: "test",
		Hosts: map[string]config.HostConfig{
			"local": {
				Transport: "local",
				Agents: map[string]config.HostAgentOverride{
					"cfg-fail":            {Command: cfgFailBin},
					"wrapper-missing-dep": {Command: wrapperMissingDepBin},
					"non-exec":            {Command: nonExecBin},
					"agent-self-err":      {Command: "agent-self-err", Env: pathEnv},
					"bad-shebang":         {Command: badShebangBin},
					"non-exec-on-path":    {Command: "non-exec-on-path", Env: pathEnv},
					"missing-path":        {Command: missingPathBin},
					"missing-bare":        {Command: "definitely-absent-agent-cmd-xyz", Env: pathEnv},
				},
			},
		},
		Agents: map[string]config.AgentConfig{
			"cfg-fail":            {},
			"wrapper-missing-dep": {},
			"non-exec":            {},
			"agent-self-err":      {},
			"bad-shebang":         {},
			"non-exec-on-path":    {},
			"missing-path":        {},
			"missing-bare":        {},
		},
	}

	checker, err := NewChecker(CheckerOptions{
		Store:  st,
		Config: cfg,
		Provider: &fakeHostProvider{
			hosts: map[string]adapter.FileAccess{
				"local": adapter.NewLocalFileAccess(dir),
			},
		},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}

	res, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	wantStatuses := map[string]store.AgentVersionStatus{
		"cfg-fail":            store.AgentVersionStatusError,
		"wrapper-missing-dep": store.AgentVersionStatusError,
		"non-exec":            store.AgentVersionStatusError,
		"agent-self-err":      store.AgentVersionStatusError,
		"bad-shebang":         store.AgentVersionStatusError,
		"non-exec-on-path":    store.AgentVersionStatusError,
		"missing-path":        store.AgentVersionStatusNotFound,
		"missing-bare":        store.AgentVersionStatusNotFound,
	}
	for _, c := range res.Matrix.Entries {
		want, ok := wantStatuses[c.Agent]
		if !ok {
			continue
		}
		if c.Status != want {
			t.Errorf("agent %q status = %q (detail=%q), want %q", c.Agent, c.Status, c.ErrorDetail, want)
		}
		if strings.TrimSpace(c.ErrorDetail) == "" {
			t.Errorf("agent %q expected non-empty ErrorDetail for status %q", c.Agent, c.Status)
		}
	}
}

func TestBuildMatrix_PinMaxVersionAndStrictSemverValidation(t *testing.T) {
	now := time.Date(2026, 11, 24, 7, 45, 0, 0, time.UTC)

	// 1. Pinned host holds the highest version -> unpinned host MUST be detected as lagging.
	cfgPinMax := &config.Config{
		InstanceID: "test",
		Hosts: map[string]config.HostConfig{
			"ubuntu":   {},
			"home-win": {Agents: map[string]config.HostAgentOverride{"codex": {Pin: "0.172.0"}}},
		},
		Agents: map[string]config.AgentConfig{"codex": {}},
	}
	rowsPinMax := []*store.AgentVersion{
		{Host: "ubuntu", Agent: "codex", Version: "0.171.0", Status: store.AgentVersionStatusOK, CheckedAt: now},
		{Host: "home-win", Agent: "codex", Version: "0.172.0", Status: store.AgentVersionStatusOK, CheckedAt: now},
	}
	mPinMax := BuildMatrix(cfgPinMax, rowsPinMax)
	if !mPinMax.HasDrift || len(mPinMax.Summaries) != 1 {
		t.Fatalf("expected HasDrift=true with 1 summary, got %+v", mPinMax)
	}
	s := mPinMax.Summaries[0]
	if !s.Drifted || !s.Comparable || s.MaxVersion != "0.172.0" {
		t.Errorf("unexpected summary when pinned host is max: %+v", s)
	}
	if len(s.LaggingHosts) != 1 || s.LaggingHosts[0] != "ubuntu" {
		t.Errorf("expected ubuntu to lag behind pinned max 0.172.0, got lagging=%v", s.LaggingHosts)
	}

	// 2. Strict SemVer 2.0.0 rejection cases: invalid semver must NEVER be ordered.
	invalidSemvers := []string{
		"01.2.3",
		"1.02.3",
		"1.2.03",
		"1.2.3-",
		"1.2.3-01",
		"1.2.3-alpha.01",
		"1.2.3-alpha..1",
		"1.2.3-.alpha",
		"1.2.3-alpha.",
		"1.2.3+",
		"1.2.3+build..1",
		"1.2.3+.build",
		"1.2.3+build.",
	}
	for _, bad := range invalidSemvers {
		if _, ok := ParseSemver(bad); ok {
			t.Errorf("ParseSemver(%q) = true, want false (invalid SemVer 2.0.0)", bad)
		}
		if norm := NormalizeVersion("tool " + bad); norm == "1.2.3" {
			t.Errorf("NormalizeVersion(%q) must not strip invalid suffix to 1.2.3, got %q", "tool "+bad, norm)
		}
		cfgBad := &config.Config{
			InstanceID: "test",
			Hosts:      map[string]config.HostConfig{"h1": {}, "h2": {}},
			Agents:     map[string]config.AgentConfig{"tool": {}},
		}
		rowsBad := []*store.AgentVersion{
			{Host: "h1", Agent: "tool", Version: bad, Status: store.AgentVersionStatusOK, CheckedAt: now},
			{Host: "h2", Agent: "tool", Version: "1.2.4", Status: store.AgentVersionStatusOK, CheckedAt: now},
		}
		mb := BuildMatrix(cfgBad, rowsBad)
		sum := mb.Summaries[0]
		if !sum.Drifted || sum.Comparable || sum.MaxVersion != "" || len(sum.LaggingHosts) != 0 {
			t.Errorf("invalid semver %q vs 1.2.4 must be drifted+incomparable without lagging hosts, got %+v", bad, sum)
		}
		for _, c := range mb.Entries {
			if c.Behind {
				t.Errorf("invalid semver %q must not mark any cell Behind, got %+v", bad, c)
			}
		}
	}

	// 3. Valid SemVer 2.0.0 boundary cases.
	validSemvers := []string{
		"0.0.0",
		"1.2.3-0",
		"1.2.3-01a",
		"1.2.3+001",
		"v1.2.3-alpha.1+build.007",
	}
	for _, good := range validSemvers {
		if _, ok := ParseSemver(good); !ok {
			t.Errorf("ParseSemver(%q) = false, want true", good)
		}
	}
}

func TestChecker_EventPersistenceAtomicityAndConcurrency(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "atomic.db")
	st, err := store.Open(ctx, dbPath, store.Options{InstanceID: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		InstanceID: "test",
		Hosts: map[string]config.HostConfig{
			"h1": {},
			"h2": {Agents: map[string]config.HostAgentOverride{"codex": {Pin: "0.169.0"}}},
		},
		Agents: map[string]config.AgentConfig{"codex": {}},
	}

	h1Ver := "0.169.0"
	h2Ver := "0.169.0"
	var mu sync.Mutex
	provider := &fakeHostProvider{
		hosts: map[string]adapter.FileAccess{
			"h1": &fakeFileAccess{runFn: func(ctx context.Context, cmd string) (adapter.ExecResult, error) {
				mu.Lock()
				v := h1Ver
				mu.Unlock()
				return adapter.ExecResult{ExitCode: 0, Output: "codex-cli " + v + "\n"}, nil
			}},
			"h2": &fakeFileAccess{runFn: func(ctx context.Context, cmd string) (adapter.ExecResult, error) {
				mu.Lock()
				v := h2Ver
				mu.Unlock()
				return adapter.ExecResult{ExitCode: 0, Output: "codex-cli " + v + "\n"}, nil
			}},
		},
	}

	now := time.Date(2026, 11, 24, 8, 0, 0, 0, time.UTC)
	var hooked int
	checker, err := NewChecker(CheckerOptions{
		Store:    st,
		Config:   cfg,
		Provider: provider,
		Now:      func() time.Time { return now },
		OnEvent:  func(ctx context.Context, ev *store.AgentVersionEvent) { hooked++ },
	})
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}

	// Initial aligned check: both hosts at 0.169.0 (no drift, 0 events).
	if res, err := checker.Check(ctx); err != nil {
		t.Fatalf("initial Check: %v", err)
	} else if len(res.Events) != 0 {
		t.Fatalf("expected 0 initial events, got %+v", res.Events)
	}

	// Inject a SQLite trigger that aborts INSERT into agent_version_events to simulate
	// an interruption/failure after agent_versions upsert before event persistence completes.
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer rawDB.Close()
	if _, err := rawDB.ExecContext(ctx, `
		CREATE TRIGGER fail_event_insert BEFORE INSERT ON agent_version_events
		BEGIN
			SELECT RAISE(ABORT, 'simulated event write failure');
		END;
	`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Bump h1 to 0.171.0 (h2 remains pinned at 0.169.0 -> should produce version_changed + drift_started).
	mu.Lock()
	h1Ver = "0.171.0"
	mu.Unlock()
	now = now.Add(time.Minute)

	if _, err := checker.Check(ctx); err == nil {
		t.Fatal("expected Check to fail while event insert trigger is active")
	}
	if hooked != 0 {
		t.Errorf("OnEvent must not be called when transaction rolls back, got %d calls", hooked)
	}

	// Verify agent_versions rolled back to 0.169.0 so the transition is NOT lost.
	gotH1, err := st.GetAgentVersion(ctx, "h1", "codex")
	if err != nil {
		t.Fatalf("GetAgentVersion: %v", err)
	}
	if gotH1.Version != "0.169.0" {
		t.Fatalf("agent_versions must roll back on event write failure, got %q want 0.169.0", gotH1.Version)
	}

	// Remove trigger and retry: BOTH agent_versions and events (version_changed + drift_started) must commit.
	if _, err := rawDB.ExecContext(ctx, `DROP TRIGGER fail_event_insert`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	now = now.Add(time.Minute)
	res, err := checker.Check(ctx)
	if err != nil {
		t.Fatalf("Check after recovery: %v", err)
	}
	var sawVerChanged, sawDriftStarted bool
	for _, ev := range res.Events {
		if ev.EventType == store.AgentEventVersionChanged && ev.Host == "h1" && ev.OldVersion == "0.169.0" && ev.NewVersion == "0.171.0" {
			sawVerChanged = true
		}
		if ev.EventType == store.AgentEventDriftStarted && ev.Agent == "codex" {
			sawDriftStarted = true
		}
	}
	if !sawVerChanged || !sawDriftStarted {
		t.Fatalf("expected both version_changed and drift_started after retry, got %+v", res.Events)
	}
	if hooked != len(res.Events) {
		t.Errorf("expected OnEvent called %d times after commit, got %d", len(res.Events), hooked)
	}

	// Concurrency check: two separate Checkers racing on the same transition (0.171.0 -> 0.172.0)
	// must record the version_changed event exactly once.
	mu.Lock()
	h1Ver = "0.172.0"
	mu.Unlock()
	now = now.Add(time.Minute)

	checker2, err := NewChecker(CheckerOptions{
		Store:    st,
		Config:   cfg,
		Provider: provider,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewChecker 2: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, e := checker.Check(ctx)
		errs <- e
	}()
	go func() {
		defer wg.Done()
		_, e := checker2.Check(ctx)
		errs <- e
	}()
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatalf("concurrent Check error: %v", e)
		}
	}

	allEvents, err := st.ListAgentVersionEvents(ctx, store.AgentVersionEventsFilter{
		Agent:     "codex",
		Host:      "h1",
		EventType: store.AgentEventVersionChanged,
	})
	if err != nil {
		t.Fatalf("ListAgentVersionEvents: %v", err)
	}
	count172 := 0
	for _, ev := range allEvents {
		if ev.OldVersion == "0.171.0" && ev.NewVersion == "0.172.0" {
			count172++
		}
	}
	if count172 != 1 {
		t.Errorf("expected exactly 1 version_changed event for 0.171.0->0.172.0 under concurrent checks, got %d", count172)
	}
}

package agentver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
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
		// codex: ubuntu 0.171.0, ubuntu-vm 0.171.0, home-win 0.169.0 (PINNED) -> NO DRIFT!
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

	// Verify codex pin exclusion: home-win is pinned, so NO drift and NOT lagging.
	codexSum := sumByAgent["codex"]
	if codexSum.Drifted {
		t.Errorf("expected codex Drifted=false because only differing host is pinned, got %+v", codexSum)
	}
	if len(codexSum.LaggingHosts) != 0 {
		t.Errorf("expected codex lagging_hosts empty, got %v", codexSum.LaggingHosts)
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
				return adapter.ExecResult{ExitCode: 127, Output: "sh: 1: unknown: not found"}, nil
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

	// Pass 1: all aligned (except home-win/codex which is pinned to 0.169.0).
	res1, err := checker.Check(ctx)
	if err != nil {
		t.Fatalf("Check pass 1: %v", err)
	}
	if res1.Matrix.HasDrift {
		t.Fatalf("pass 1: expected HasDrift=false (pinned host excluded), got summaries=%+v", res1.Matrix.Summaries)
	}
	if len(res1.Events) != 0 {
		t.Fatalf("pass 1: expected 0 events on clean initial probe, got %d: %+v", len(res1.Events), res1.Events)
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
			return adapter.ExecResult{ExitCode: 127, Output: "sh: 1: agy: not found"}, nil
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
	if res3.Matrix.HasDrift {
		t.Fatalf("pass 3: expected HasDrift=false after revert")
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
	if len(hooked) != len(res2.Events)+len(res3.Events) {
		t.Errorf("expected OnEvent hook called %d times, got %d", len(res2.Events)+len(res3.Events), len(hooked))
	}
}

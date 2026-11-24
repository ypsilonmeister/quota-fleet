package agentmodels

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

type fakeFileAccess struct {
	runFn func(ctx context.Context, cmd string) (adapter.ExecResult, error)
}

func (f *fakeFileAccess) Stat(ctx context.Context, path string) (adapter.FileStat, error) {
	return adapter.FileStat{}, nil
}

func (f *fakeFileAccess) ReadFile(ctx context.Context, path string) ([]byte, error) {
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
		return nil, errors.New("host not found")
	}
	return fa, nil
}

func TestParseAgyModelsOutput(t *testing.T) {
	raw := "\r\x1b[2K⠋ Fetching available models...\r⠴ Fetching available models...\rgemini-4-neon-medium           Gemini 4 Neon (Medium)\n" +
		"claude-opus-5-5-high           Claude Opus 5.5 (High)\n" +
		"gpt-oss-120b-medium            GPT-OSS 120B (Medium)\n" +
		"custom-local-model             Custom Local\n" +
		"gemini-4-neon-medium           Duplicate Should Be Ignored\n"

	items, err := ParseAgyModelsOutput(raw)
	if err != nil {
		t.Fatalf("ParseAgyModelsOutput failed: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4: %+v", len(items), items)
	}
	want := []store.AgentModelItem{
		{ID: "gemini-4-neon-medium", DisplayName: "Gemini 4 Neon (Medium)", Family: quota.ModelFamilyGemini},
		{ID: "claude-opus-5-5-high", DisplayName: "Claude Opus 5.5 (High)", Family: quota.ModelFamilyClaude},
		{ID: "gpt-oss-120b-medium", DisplayName: "GPT-OSS 120B (Medium)", Family: quota.ModelFamilyGPT},
		{ID: "custom-local-model", DisplayName: "Custom Local", Family: quota.ModelFamilyUnknown},
	}
	for i, w := range want {
		if items[i] != w {
			t.Errorf("items[%d] = %+v, want %+v", i, items[i], w)
		}
	}

	for _, diag := range []string{
		"\r⠋ Fetching available models...\n",
		"No models available\n",
		"no models available\n",
		"No models found.\n",
		"Failed to fetch models\n",
		"Authentication required. Run agy login\n",
		"ID   Display Name\n---\n",
	} {
		if got, err := ParseAgyModelsOutput(diag); err == nil {
			t.Errorf("ParseAgyModelsOutput(%q) succeeded with %+v, want error", diag, got)
		}
	}
}

func TestParseCodexModelsCache(t *testing.T) {
	raw := `{
		"models": [
			{"slug": "gpt-6.2-astra", "display_name": "GPT-6.2-Astra", "visibility": "list", "priority": 2},
			{"slug": "gpt-reserve", "display_name": "GPT-Reserve", "visibility": "hide", "priority": 1},
			{"slug": "gpt-6.2-sol", "display_name": "GPT-6.2-Sol", "visibility": "list", "priority": 1},
			{"slug": "codex-auto-review", "display_name": "Codex Auto Review", "visibility": "hide", "priority": 10}
		]
	}`
	items, err := ParseCodexModelsCache([]byte(raw))
	if err != nil {
		t.Fatalf("ParseCodexModelsCache failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2: %+v", len(items), items)
	}
	if items[0].ID != "gpt-6.2-sol" || items[0].DisplayName != "GPT-6.2-Sol" || items[0].Family != quota.ModelFamilyGPT {
		t.Errorf("items[0] = %+v", items[0])
	}
	if items[1].ID != "gpt-6.2-astra" {
		t.Errorf("items[1] = %+v", items[1])
	}
}

func TestProberPersistsCatalogAndPreservesOnFailure(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), store.Options{InstanceID: "model-probe-test"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		InstanceID: "model-probe-test",
		Hosts: map[string]config.HostConfig{
			"ubuntu": {
				OS: "linux",
				Agents: map[string]config.HostAgentOverride{
					"agy":    {},
					"codex":  {},
					"claude": {},
				},
			},
			"home-win": {
				OS: "windows",
				Agents: map[string]config.HostAgentOverride{
					"codex": {},
				},
			},
		},
		Agents: map[string]config.AgentConfig{
			"claude": {
				Models: []config.ModelConfig{
					{ID: "claude-opus-4-6", DisplayName: "Claude Opus 4.6"},
					{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6"},
				},
			},
			"codex": {
				Models: []config.ModelConfig{
					{ID: "gpt-fallback", DisplayName: "GPT Fallback"},
				},
			},
		},
	}

	failProbes := false
	provider := &fakeHostProvider{
		hosts: map[string]adapter.FileAccess{
			"ubuntu": &fakeFileAccess{
				runFn: func(ctx context.Context, cmd string) (adapter.ExecResult, error) {
					if failProbes {
						return adapter.ExecResult{ExitCode: 1, Output: "network error"}, nil
					}
					if strings.Contains(cmd, "models_cache.json") {
						return adapter.ExecResult{
							ExitCode: 0,
							Output:   `{"models":[{"slug":"gpt-6.2-sol","display_name":"GPT-6.2-Sol","visibility":"list","priority":1}]}`,
						}, nil
					}
					if strings.Contains(cmd, "agy") && strings.Contains(cmd, "models") {
						return adapter.ExecResult{
							ExitCode: 0,
							Output:   "gemini-4-carbon-medium   Gemini 4 Carbon (Medium)\nclaude-opus-5-5-high   Claude Opus 5.5 (High)\n",
						}, nil
					}
					return adapter.ExecResult{ExitCode: 1, Output: "unexpected cmd: " + cmd}, nil
				},
			},
			"home-win": &fakeFileAccess{
				runFn: func(ctx context.Context, cmd string) (adapter.ExecResult, error) {
					// Simulate missing models_cache.json on home-win -> should fall back to config catalog.
					return adapter.ExecResult{ExitCode: 44, Output: ""}, nil
				},
			},
		},
	}

	t1 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	now := t1
	prober, err := NewProber(ProberOptions{
		Store:    st,
		Config:   cfg,
		Provider: provider,
		Now:      func() time.Time { return now },
		Logger:   log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewProber: %v", err)
	}

	// First probe succeeds for ubuntu (agy/codex/claude) and falls back to config for home-win/codex.
	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("first Probe: %v", err)
	}

	agyCat, err := st.GetAgentModels(ctx, "ubuntu", "agy")
	if err != nil {
		t.Fatalf("GetAgentModels(ubuntu, agy): %v", err)
	}
	if agyCat.Status != store.AgentModelStatusOK || agyCat.Source != SourceAgyCLI || len(agyCat.Models) != 2 {
		t.Fatalf("unexpected ubuntu/agy catalog: %+v", agyCat)
	}

	codexCat, err := st.GetAgentModels(ctx, "ubuntu", "codex")
	if err != nil {
		t.Fatalf("GetAgentModels(ubuntu, codex): %v", err)
	}
	if codexCat.Status != store.AgentModelStatusOK || codexCat.Source != SourceCodexCache || len(codexCat.Models) != 1 || codexCat.Models[0].ID != "gpt-6.2-sol" {
		t.Fatalf("unexpected ubuntu/codex catalog: %+v", codexCat)
	}

	claudeCat, err := st.GetAgentModels(ctx, "ubuntu", "claude")
	if err != nil {
		t.Fatalf("GetAgentModels(ubuntu, claude): %v", err)
	}
	if claudeCat.Source != SourceConfig || len(claudeCat.Models) != 2 {
		t.Fatalf("unexpected ubuntu/claude catalog: %+v", claudeCat)
	}

	winCodexCat, err := st.GetAgentModels(ctx, "home-win", "codex")
	if err != nil {
		t.Fatalf("GetAgentModels(home-win, codex): %v", err)
	}
	if winCodexCat.Source != SourceConfig || len(winCodexCat.Models) != 1 || winCodexCat.Models[0].ID != "gpt-fallback" {
		t.Fatalf("expected home-win/codex to fall back to config catalog, got: %+v", winCodexCat)
	}

	// Second probe fails on ubuntu -> previous catalogs MUST be preserved (not wiped!).
	failProbes = true
	t2 := t1.Add(24 * time.Hour)
	now = t2
	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("second Probe: %v", err)
	}

	agyAfterFail, err := st.GetAgentModels(ctx, "ubuntu", "agy")
	if err != nil {
		t.Fatalf("GetAgentModels(ubuntu, agy) after fail: %v", err)
	}
	if agyAfterFail.Status != store.AgentModelStatusError || agyAfterFail.ErrorDetail == "" {
		t.Errorf("expected status=error and error_detail, got %+v", agyAfterFail)
	}
	if len(agyAfterFail.Models) != 2 || agyAfterFail.Models[0].ID != "gemini-4-carbon-medium" {
		t.Errorf("previous agy catalog was NOT preserved on failure: %+v", agyAfterFail)
	}
	if agyAfterFail.UpdatedAt == nil || !agyAfterFail.UpdatedAt.Equal(t1) {
		t.Errorf("expected updated_at to remain %v, got %v", t1, agyAfterFail.UpdatedAt)
	}

	codexAfterFail, err := st.GetAgentModels(ctx, "ubuntu", "codex")
	if err != nil {
		t.Fatalf("GetAgentModels(ubuntu, codex) after fail: %v", err)
	}
	if codexAfterFail.Status != store.AgentModelStatusError {
		t.Errorf("expected codex status=error, got %+v", codexAfterFail)
	}
	if len(codexAfterFail.Models) != 1 || codexAfterFail.Models[0].ID != "gpt-6.2-sol" {
		t.Errorf("previous codex catalog was NOT preserved on failure: %+v", codexAfterFail)
	}
}

func TestProberPreservesAgyCatalogOnDiagnosticMessageWithExitZero(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), store.Options{InstanceID: "agy-diag-test"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		InstanceID: "agy-diag-test",
		Hosts: map[string]config.HostConfig{
			"ubuntu": {
				OS: "linux",
				Agents: map[string]config.HostAgentOverride{
					"agy": {},
				},
			},
		},
	}

	output := "gemini-4-neon-medium           Gemini 4 Neon (Medium)\nclaude-opus-5-5-high           Claude Opus 5.5 (High)\n"
	provider := &fakeHostProvider{
		hosts: map[string]adapter.FileAccess{
			"ubuntu": &fakeFileAccess{
				runFn: func(ctx context.Context, cmd string) (adapter.ExecResult, error) {
					return adapter.ExecResult{ExitCode: 0, Output: output}, nil
				},
			},
		},
	}

	t1 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	now := t1
	prober, err := NewProber(ProberOptions{
		Store:    st,
		Config:   cfg,
		Provider: provider,
		Now:      func() time.Time { return now },
		Logger:   log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewProber: %v", err)
	}

	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("first Probe: %v", err)
	}

	// Second probe exits 0 but only returns a diagnostic empty-list message.
	output = "No models available\n"
	now = t1.Add(24 * time.Hour)
	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("second Probe: %v", err)
	}

	got, err := st.GetAgentModels(ctx, "ubuntu", "agy")
	if err != nil {
		t.Fatalf("GetAgentModels: %v", err)
	}
	if got.Status != store.AgentModelStatusError {
		t.Errorf("status = %q, want error", got.Status)
	}
	if len(got.Models) != 2 || got.Models[0].ID != "gemini-4-neon-medium" || got.Models[1].ID != "claude-opus-5-5-high" {
		t.Errorf("previous agy catalog must be preserved when exit-0 output is diagnostic only, got %+v", got.Models)
	}
	if got.UpdatedAt == nil || !got.UpdatedAt.Equal(t1) {
		t.Errorf("updated_at = %v, want preserved %v", got.UpdatedAt, t1)
	}
}

func TestProberClearsClaudeCatalogWhenRemovedFromConfig(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), store.Options{InstanceID: "claude-clear-test"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		InstanceID: "claude-clear-test",
		Hosts: map[string]config.HostConfig{
			"ubuntu": {
				OS: "linux",
				Agents: map[string]config.HostAgentOverride{
					"claude": {},
				},
			},
		},
		Agents: map[string]config.AgentConfig{
			"claude": {
				Models: []config.ModelConfig{
					{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6"},
				},
			},
		},
	}

	t1 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	now := t1
	prober, err := NewProber(ProberOptions{
		Store:  st,
		Config: cfg,
		Now:    func() time.Time { return now },
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewProber: %v", err)
	}

	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("first Probe: %v", err)
	}
	first, err := st.GetAgentModels(ctx, "ubuntu", "claude")
	if err != nil || len(first.Models) != 1 {
		t.Fatalf("first claude catalog = %+v, err=%v", first, err)
	}

	// Remove all claude models from config and probe again.
	cfg.Agents["claude"] = config.AgentConfig{}
	now = t1.Add(time.Hour)
	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("second Probe: %v", err)
	}
	second, err := st.GetAgentModels(ctx, "ubuntu", "claude")
	if err != nil {
		t.Fatalf("GetAgentModels after clear: %v", err)
	}
	if len(second.Models) != 0 {
		t.Errorf("expected claude catalog in DB to be cleared, got %+v", second.Models)
	}
}

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
		"gemini-4-neon-medium           Gemini 4 Neon (Medium) Duplicate\n"

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

	// Non-TTY `agy models` combined output (stderr status line + tab-separated stdout rows).
	nonTTYRaw := "Fetching available models...\n" +
		"gemini-4-neon-medium\tGemini 4 Neon (Medium)\n" +
		"gemini-pro-orchestrator\tOrchestrator (Carbon)\n" +
		"gemini-3.8-flash-high\tGemini 3.8 Flash (High)\n" +
		"claude-opus-5-5-high\tClaude Opus 5.5 (High)\n" +
		"gpt-oss-120b-medium\tGPT-OSS 120B (Medium)\n"
	nonTTYItems, err := ParseAgyModelsOutput(nonTTYRaw)
	if err != nil {
		t.Fatalf("ParseAgyModelsOutput(non-TTY) failed: %v", err)
	}
	wantNonTTY := []store.AgentModelItem{
		{ID: "gemini-4-neon-medium", DisplayName: "Gemini 4 Neon (Medium)", Family: quota.ModelFamilyGemini},
		{ID: "gemini-pro-orchestrator", DisplayName: "Orchestrator (Carbon)", Family: quota.ModelFamilyGemini},
		{ID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash (High)", Family: quota.ModelFamilyGemini},
		{ID: "claude-opus-5-5-high", DisplayName: "Claude Opus 5.5 (High)", Family: quota.ModelFamilyClaude},
		{ID: "gpt-oss-120b-medium", DisplayName: "GPT-OSS 120B (Medium)", Family: quota.ModelFamilyGPT},
	}
	if len(nonTTYItems) != len(wantNonTTY) {
		t.Fatalf("non-TTY got %d items, want %d: %+v", len(nonTTYItems), len(wantNonTTY), nonTTYItems)
	}
	for i, w := range wantNonTTY {
		if nonTTYItems[i] != w {
			t.Errorf("nonTTYItems[%d] = %+v, want %+v", i, nonTTYItems[i], w)
		}
	}

	// PTY `agy models` output (spinner with \r\x1b[K clear + space-padded CRLF rows).
	ptyRaw := "\r⠋ Fetching available models...\r⠙ Fetching available models...\r\x1b[Kgemini-4-neon-high             Gemini 4 Neon (High)\r\n" +
		"gemini-pro-orchestrator-argon  Orchestrator (Argon)\r\n" +
		"claude-sonnet-5-5-high         Claude Sonnet 5.5 (High)\r\n"
	ptyItems, err := ParseAgyModelsOutput(ptyRaw)
	if err != nil {
		t.Fatalf("ParseAgyModelsOutput(PTY) failed: %v", err)
	}
	if len(ptyItems) != 3 || ptyItems[0].ID != "gemini-4-neon-high" || ptyItems[1].ID != "gemini-pro-orchestrator-argon" || ptyItems[2].ID != "claude-sonnet-5-5-high" {
		t.Fatalf("unexpected PTY items: %+v", ptyItems)
	}

	for _, diag := range []string{
		"\r⠋ Fetching available models...\n",
		"No models available\n",
		"no models available\n",
		"No models found.\n",
		"Failed to fetch models\n",
		"Authentication required. Run agy login\n",
		"ID   Display Name\n---\n",
		"error-code  authentication required\n",
		"error-code\tauthentication required\n",
		"error-code  Authentication required\n",
		"error-code\tAuthentication required\n",
		"Fetching available models...\nerror-code  authentication required\n",
		"gemini-4-neon-medium\tAuthentication required\n",
		"gemini-4-neon-medium  Authentication required\n",
		"gemini-4-neon-high\tCatalog retrieval interrupted\n",
		"gemini-4-neon-medium\tGemini 4 Neon (Medium)\nerror-code\tAuthentication required\n",
		"gemini-4-neon-medium\tGemini 4 Neon (Medium)\ngemini-4-neon-high\tAuthentication required\n",
		"gemini-4-neon-medium\tGemini 4 Neon (Medium)\ngemini-4-neon-high\tCatalog retrieval interrupted\n",
		"gemini-4-neon-medium\tGemini 4 Neon (Medium)\nclaude-opus-5-5-high\tClaude Opus 5.5 (High)\nWarning: incomplete catalog\n",
		"gemini-4-neon-medium\tGemini 4 Neon (Medium)\nclaude-opus-5-5-high  Claude Opus 5.5 (High)\n",
		"err-401\n",
		"gemini-4-neon-medium\n",
		"Usage: agy models [flags]\n\nList available models\n\nFlags:\n  -h      Show help\n  --help  Show help\n",
	} {
		if got, err := ParseAgyModelsOutput(diag); err == nil {
			t.Errorf("ParseAgyModelsOutput(%q) succeeded with %+v, want error", diag, got)
		}
	}

	// Unknown-prefix model catalog must be accepted with family=unknown (docs/11 §3).
	unknownOnlyRaw := "new-provider-v1\tNew Provider V1\nnew-provider-v2\tnew-provider-v2\n"
	unknownItems, err := ParseAgyModelsOutput(unknownOnlyRaw)
	if err != nil {
		t.Fatalf("ParseAgyModelsOutput(unknown-only) failed: %v", err)
	}
	wantUnknown := []store.AgentModelItem{
		{ID: "new-provider-v1", DisplayName: "New Provider V1", Family: quota.ModelFamilyUnknown},
		{ID: "new-provider-v2", DisplayName: "new-provider-v2", Family: quota.ModelFamilyUnknown},
	}
	if len(unknownItems) != len(wantUnknown) {
		t.Fatalf("unknown-only got %d items, want %d: %+v", len(unknownItems), len(wantUnknown), unknownItems)
	}
	for i, w := range wantUnknown {
		if unknownItems[i] != w {
			t.Errorf("unknownItems[%d] = %+v, want %+v", i, unknownItems[i], w)
		}
	}

	// Single-model valid lists (known and unknown prefix) must be accepted (docs/11 §3).
	singleKnown, err := ParseAgyModelsOutput("gemini-4-neon-medium\tGemini 4 Neon (Medium)\n")
	if err != nil {
		t.Fatalf("ParseAgyModelsOutput(single known) failed: %v", err)
	}
	if len(singleKnown) != 1 || singleKnown[0].ID != "gemini-4-neon-medium" || singleKnown[0].DisplayName != "Gemini 4 Neon (Medium)" {
		t.Fatalf("unexpected singleKnown: %+v", singleKnown)
	}

	singleUnknown, err := ParseAgyModelsOutput("new-provider-v1\tNew Provider V1\n")
	if err != nil {
		t.Fatalf("ParseAgyModelsOutput(single unknown) failed: %v", err)
	}
	if len(singleUnknown) != 1 || singleUnknown[0].ID != "new-provider-v1" || singleUnknown[0].Family != quota.ModelFamilyUnknown {
		t.Fatalf("unexpected singleUnknown: %+v", singleUnknown)
	}

	// Tab-separated display names must preserve internal consecutive spaces.
	doubleSpaceRaw := "gemini-4-neon-medium\tGemini 4  Neon (Medium)\nclaude-opus-5-5-high\tClaude Opus 5.5 (High)\n"
	doubleSpaceItems, err := ParseAgyModelsOutput(doubleSpaceRaw)
	if err != nil {
		t.Fatalf("ParseAgyModelsOutput(tab with double space) failed: %v", err)
	}
	if len(doubleSpaceItems) != 2 || doubleSpaceItems[0].DisplayName != "Gemini 4  Neon (Medium)" {
		t.Fatalf("unexpected doubleSpaceItems: %+v", doubleSpaceItems)
	}
}

// Captured real `agy models` samples (non-TTY combined stderr+stdout and PTY output).
const (
	realAgyModelsNonTTY = "Fetching available models...\n" +
		"gemini-4-neon-high\tGemini 4 Neon (High)\n" +
		"gemini-4-neon-xhigh\tGemini 4 Neon (XHigh)\n" +
		"gemini-4-neon-medium\tGemini 4 Neon (Medium)\n" +
		"gemini-4-neon-low\tGemini 4 Neon (Low)\n" +
		"gemini-4-carbon-medium\tGemini 4 Carbon (Medium)\n" +
		"gemini-4-carbon-high\tGemini 4 Carbon (High)\n" +
		"gemini-4-carbon-low\tGemini 4 Carbon (Low)\n" +
		"gemini-4-argon-medium\tGemini 4 Argon (Medium)\n" +
		"gemini-4-argon-high\tGemini 4 Argon (High)\n" +
		"gemini-4-argon-low\tGemini 4 Argon (Low)\n" +
		"gemini-4-hydrogen-high\tGemini 4 Hydrogen (High)\n" +
		"gemini-4-hydrogen-xhigh\tGemini 4 Hydrogen (XHigh)\n" +
		"gemini-4-hydrogen-medium\tGemini 4 Hydrogen (Medium)\n" +
		"gemini-4-hydrogen-low\tGemini 4 Hydrogen (Low)\n" +
		"gemini-pro-orchestrator-neon\tOrchestrator (Neon)\n" +
		"gemini-pro-orchestrator\tOrchestrator (Carbon)\n" +
		"gemini-pro-orchestrator-argon\tOrchestrator (Argon)\n" +
		"gemini-3.8-flash-high\tGemini 3.8 Flash (High)\n" +
		"gemini-3.8-flash-medium\tGemini 3.8 Flash (Medium)\n" +
		"gemini-3.8-flash-low\tGemini 3.8 Flash (Low)\n" +
		"claude-opus-5-5-low\tClaude Opus 5.5 (Low)\n" +
		"claude-opus-5-5-medium\tClaude Opus 5.5 (Medium)\n" +
		"claude-opus-5-5-high\tClaude Opus 5.5 (High)\n" +
		"claude-sonnet-5-5-low\tClaude Sonnet 5.5 (Low)\n" +
		"claude-sonnet-5-5-medium\tClaude Sonnet 5.5 (Medium)\n" +
		"claude-sonnet-5-5-high\tClaude Sonnet 5.5 (High)\n" +
		"gpt-oss-120b-medium\tGPT-OSS 120B (Medium)\n"

	realAgyModelsPTY = "\r⠋ Fetching available models...\r⠙ Fetching available models...\r⠹ Fetching available models...\r\x1b[K" +
		"gemini-4-neon-high             Gemini 4 Neon (High)\r\n" +
		"gemini-4-neon-xhigh            Gemini 4 Neon (XHigh)\r\n" +
		"gemini-4-neon-medium           Gemini 4 Neon (Medium)\r\n" +
		"gemini-4-neon-low              Gemini 4 Neon (Low)\r\n" +
		"gemini-4-carbon-medium         Gemini 4 Carbon (Medium)\r\n" +
		"gemini-4-carbon-high           Gemini 4 Carbon (High)\r\n" +
		"gemini-4-carbon-low            Gemini 4 Carbon (Low)\r\n" +
		"gemini-4-argon-medium          Gemini 4 Argon (Medium)\r\n" +
		"gemini-4-argon-high            Gemini 4 Argon (High)\r\n" +
		"gemini-4-argon-low             Gemini 4 Argon (Low)\r\n" +
		"gemini-4-hydrogen-high         Gemini 4 Hydrogen (High)\r\n" +
		"gemini-4-hydrogen-xhigh        Gemini 4 Hydrogen (XHigh)\r\n" +
		"gemini-4-hydrogen-medium       Gemini 4 Hydrogen (Medium)\r\n" +
		"gemini-4-hydrogen-low          Gemini 4 Hydrogen (Low)\r\n" +
		"gemini-pro-orchestrator-neon   Orchestrator (Neon)\r\n" +
		"gemini-pro-orchestrator        Orchestrator (Carbon)\r\n" +
		"gemini-pro-orchestrator-argon  Orchestrator (Argon)\r\n" +
		"gemini-3.8-flash-high          Gemini 3.8 Flash (High)\r\n" +
		"gemini-3.8-flash-medium        Gemini 3.8 Flash (Medium)\r\n" +
		"gemini-3.8-flash-low           Gemini 3.8 Flash (Low)\r\n" +
		"claude-opus-5-5-low            Claude Opus 5.5 (Low)\r\n" +
		"claude-opus-5-5-medium         Claude Opus 5.5 (Medium)\r\n" +
		"claude-opus-5-5-high           Claude Opus 5.5 (High)\r\n" +
		"claude-sonnet-5-5-low          Claude Sonnet 5.5 (Low)\r\n" +
		"claude-sonnet-5-5-medium       Claude Sonnet 5.5 (Medium)\r\n" +
		"claude-sonnet-5-5-high         Claude Sonnet 5.5 (High)\r\n" +
		"gpt-oss-120b-medium            GPT-OSS 120B (Medium)\r\n"
)

func TestProberImportsRealAgySamplesAndUnknownFamilyCatalog(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), store.Options{InstanceID: "agy-real-samples"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		InstanceID: "agy-real-samples",
		Hosts: map[string]config.HostConfig{
			"ubuntu": {
				OS: "linux",
				Agents: map[string]config.HostAgentOverride{
					"agy": {},
				},
			},
		},
	}

	output := realAgyModelsNonTTY
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

	// 1. Real non-TTY sample: all 27 models imported.
	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("non-TTY Probe: %v", err)
	}
	nonTTYCat, err := st.GetAgentModels(ctx, "ubuntu", "agy")
	if err != nil {
		t.Fatalf("GetAgentModels(non-TTY): %v", err)
	}
	if nonTTYCat.Status != store.AgentModelStatusOK || len(nonTTYCat.Models) != 27 {
		t.Fatalf("non-TTY catalog = status=%q len=%d, want ok and 27", nonTTYCat.Status, len(nonTTYCat.Models))
	}

	// 2. Real PTY sample: all 27 models imported identically in exact order.
	output = realAgyModelsPTY
	now = t1.Add(time.Hour)
	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("PTY Probe: %v", err)
	}
	ptyCat, err := st.GetAgentModels(ctx, "ubuntu", "agy")
	if err != nil {
		t.Fatalf("GetAgentModels(PTY): %v", err)
	}
	if ptyCat.Status != store.AgentModelStatusOK || len(ptyCat.Models) != len(nonTTYCat.Models) {
		t.Fatalf("PTY catalog = status=%q len=%d, want ok and %d", ptyCat.Status, len(ptyCat.Models), len(nonTTYCat.Models))
	}
	for i := range nonTTYCat.Models {
		if ptyCat.Models[i] != nonTTYCat.Models[i] {
			t.Errorf("PTY model[%d] = %+v, want %+v", i, ptyCat.Models[i], nonTTYCat.Models[i])
		}
	}

	// 3. Unknown-prefix catalog (docs/11 §3) + lowercase display name: saved as ok with family=unknown.
	output = "new-provider-v1\tNew Provider V1\nnew-provider-v2\tnew-provider-v2\n"
	now = t1.Add(2 * time.Hour)
	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("unknown-family Probe: %v", err)
	}
	unkCat, err := st.GetAgentModels(ctx, "ubuntu", "agy")
	if err != nil {
		t.Fatalf("GetAgentModels(unknown): %v", err)
	}
	if unkCat.Status != store.AgentModelStatusOK || len(unkCat.Models) != 2 {
		t.Fatalf("unknown catalog = %+v, want status=ok and 2 models", unkCat)
	}
	if unkCat.Models[0].ID != "new-provider-v1" || unkCat.Models[0].Family != quota.ModelFamilyUnknown ||
		unkCat.Models[1].ID != "new-provider-v2" || unkCat.Models[1].DisplayName != "new-provider-v2" || unkCat.Models[1].Family != quota.ModelFamilyUnknown {
		t.Errorf("unexpected unknown-family models: %+v", unkCat.Models)
	}

	// 4. Single-model valid catalog (known ID) and tab-separated display name with consecutive spaces.
	output = "gemini-4-neon-medium\tGemini 4  Neon (Medium)\n"
	now = t1.Add(3 * time.Hour)
	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("single-model Probe: %v", err)
	}
	singleCat, err := st.GetAgentModels(ctx, "ubuntu", "agy")
	if err != nil {
		t.Fatalf("GetAgentModels(single): %v", err)
	}
	if singleCat.Status != store.AgentModelStatusOK || len(singleCat.Models) != 1 ||
		singleCat.Models[0].ID != "gemini-4-neon-medium" || singleCat.Models[0].DisplayName != "Gemini 4  Neon (Medium)" {
		t.Errorf("unexpected single-model catalog: %+v", singleCat)
	}

	// 5. Single-model unknown-prefix catalog: saved as ok with family=unknown.
	output = "new-provider-v1\tNew Provider V1\n"
	now = t1.Add(4 * time.Hour)
	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("single unknown-family Probe: %v", err)
	}
	singleUnkCat, err := st.GetAgentModels(ctx, "ubuntu", "agy")
	if err != nil {
		t.Fatalf("GetAgentModels(single unknown): %v", err)
	}
	if singleUnkCat.Status != store.AgentModelStatusOK || len(singleUnkCat.Models) != 1 ||
		singleUnkCat.Models[0].ID != "new-provider-v1" || singleUnkCat.Models[0].Family != quota.ModelFamilyUnknown {
		t.Errorf("unexpected single unknown-family catalog: %+v", singleUnkCat)
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
	cases := []struct {
		name   string
		output string
	}{
		{
			name:   "plain prose empty message",
			output: "No models available\n",
		},
		{
			name:   "two-space separated diagnostic",
			output: "error-code  authentication required\n",
		},
		{
			name:   "tab separated diagnostic",
			output: "error-code\tauthentication required\n",
		},
		{
			name:   "two-space separated capitalized diagnostic",
			output: "error-code  Authentication required\n",
		},
		{
			name:   "tab separated capitalized diagnostic",
			output: "error-code\tAuthentication required\n",
		},
		{
			name:   "known model ID with tab diagnostic",
			output: "gemini-4-neon-medium\tAuthentication required\n",
		},
		{
			name:   "known model ID with two-space diagnostic",
			output: "gemini-4-neon-medium  Authentication required\n",
		},
		{
			name:   "valid row mixed with tabular error diagnostic",
			output: "gemini-4-neon-medium\tGemini 4 Neon (Medium)\nerror-code\tAuthentication required\n",
		},
		{
			name:   "valid row mixed with known-ID diagnostic",
			output: "gemini-4-neon-medium\tGemini 4 Neon (Medium)\ngemini-4-neon-high\tAuthentication required\n",
		},
		{
			name:   "valid row mixed with unlisted diagnostic text",
			output: "gemini-4-neon-medium\tGemini 4 Neon (Medium)\ngemini-4-neon-high\tCatalog retrieval interrupted\n",
		},
		{
			name:   "single-line known-ID with unlisted diagnostic text",
			output: "gemini-4-neon-high\tCatalog retrieval interrupted\n",
		},
		{
			name:   "valid rows mixed with plain-text diagnostic line",
			output: "gemini-4-neon-medium\tGemini 4 Neon (Medium)\nclaude-opus-5-5-high\tClaude Opus 5.5 (High)\nWarning: incomplete catalog\n",
		},
		{
			name:   "mixed tab and space separators",
			output: "gemini-4-neon-medium\tGemini 4 Neon (Medium)\nclaude-opus-5-5-high  Claude Opus 5.5 (High)\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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

			output := "Fetching available models...\ngemini-4-neon-medium\tGemini 4 Neon (Medium)\nclaude-opus-5-5-high\tClaude Opus 5.5 (High)\n"
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

			// Second probe exits 0 but only returns a diagnostic message.
			output = tc.output
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
			if got.ErrorDetail == "" {
				t.Errorf("error_detail is empty, want non-empty")
			}
			if len(got.Models) != 2 || got.Models[0].ID != "gemini-4-neon-medium" || got.Models[1].ID != "claude-opus-5-5-high" {
				t.Errorf("previous agy catalog must be preserved when exit-0 output is diagnostic only, got %+v", got.Models)
			}
			if got.UpdatedAt == nil || !got.UpdatedAt.Equal(t1) {
				t.Errorf("updated_at = %v, want preserved %v", got.UpdatedAt, t1)
			}
		})
	}
}

func TestProberUsesSeparatedAgyStdoutAndStderr(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), store.Options{InstanceID: "agy-streams-test"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		InstanceID: "agy-streams-test",
		Hosts: map[string]config.HostConfig{
			"ubuntu": {
				OS: "linux",
				Agents: map[string]config.HostAgentOverride{
					"agy": {},
				},
			},
		},
	}

	var nextRes adapter.ExecResult
	provider := &fakeHostProvider{
		hosts: map[string]adapter.FileAccess{
			"ubuntu": &fakeFileAccess{
				runFn: func(ctx context.Context, cmd string) (adapter.ExecResult, error) {
					return nextRes, nil
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

	// 1. Clean non-TTY execution: progress only on stderr, tab-separated models on stdout.
	nextRes = adapter.ExecResult{
		ExitCode: 0,
		Stdout:   "gemini-4-neon-medium\tGemini 4 Neon (Medium)\nclaude-opus-5-5-high\tClaude Opus 5.5 (High)\n",
		Stderr:   "Fetching available models...\n",
		Output:   "Fetching available models...\ngemini-4-neon-medium\tGemini 4 Neon (Medium)\nclaude-opus-5-5-high\tClaude Opus 5.5 (High)\n",
	}
	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("first Probe: %v", err)
	}
	got, err := st.GetAgentModels(ctx, "ubuntu", "agy")
	if err != nil {
		t.Fatalf("GetAgentModels: %v", err)
	}
	if got.Status != store.AgentModelStatusOK || len(got.Models) != 2 {
		t.Fatalf("first catalog = %+v, want ok with 2 models", got)
	}

	// 2. Exit 0 with valid stdout row, but unexpected diagnostic text on stderr -> preserve previous catalog.
	nextRes = adapter.ExecResult{
		ExitCode: 0,
		Stdout:   "gemini-4-neon-medium\tGemini 4 Neon (Medium)\n",
		Stderr:   "Fetching available models...\nCatalog retrieval interrupted\n",
		Output:   "gemini-4-neon-medium\tGemini 4 Neon (Medium)\nFetching available models...\nCatalog retrieval interrupted\n",
	}
	now = t1.Add(24 * time.Hour)
	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("second Probe: %v", err)
	}
	got, err = st.GetAgentModels(ctx, "ubuntu", "agy")
	if err != nil {
		t.Fatalf("GetAgentModels after stderr diag: %v", err)
	}
	if got.Status != store.AgentModelStatusError || got.ErrorDetail == "" {
		t.Errorf("expected error status on unexpected stderr, got %+v", got)
	}
	if len(got.Models) != 2 || got.Models[1].ID != "claude-opus-5-5-high" {
		t.Errorf("previous catalog must be preserved on unexpected stderr, got %+v", got.Models)
	}
	if got.UpdatedAt == nil || !got.UpdatedAt.Equal(t1) {
		t.Errorf("updated_at = %v, want preserved %v", got.UpdatedAt, t1)
	}

	// 3. Exit 0 with empty stdout and progress-only stderr -> preserve previous catalog.
	nextRes = adapter.ExecResult{
		ExitCode: 0,
		Stdout:   "",
		Stderr:   "Fetching available models...\n",
		Output:   "Fetching available models...\n",
	}
	now = t1.Add(48 * time.Hour)
	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("third Probe: %v", err)
	}
	got, err = st.GetAgentModels(ctx, "ubuntu", "agy")
	if err != nil {
		t.Fatalf("GetAgentModels after empty stdout: %v", err)
	}
	if got.Status != store.AgentModelStatusError || len(got.Models) != 2 || got.UpdatedAt == nil || !got.UpdatedAt.Equal(t1) {
		t.Errorf("previous catalog must be preserved on empty stdout, got %+v", got)
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

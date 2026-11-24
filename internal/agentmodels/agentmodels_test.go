package agentmodels

import (
	"bytes"
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

func TestParseAgyModelsJSON(t *testing.T) {
	raw := `{
		"conversation_id": "",
		"status": "SUCCESS",
		"response": "gemini-4-neon-medium\tGemini 4 Neon (Medium)\n",
		"command": {
			"name": "models",
			"data": {
				"models": [
					{"id": "gemini-4-neon-medium", "label": "Gemini 4 Neon (Medium)"},
					{"id": "claude-opus-5-5-high", "label": "Claude Opus 5.5 (High)"},
					{"id": "gpt-oss-120b-medium", "label": "GPT-OSS 120B (Medium)"},
					{"id": "custom-local-model", "label": "Custom Local"},
					{"id": "no-label-model", "label": ""},
					{"id": "gemini-4-neon-medium", "label": "Duplicate Ignored"}
				]
			}
		}
	}`

	items, err := ParseAgyModelsJSON([]byte(raw))
	if err != nil {
		t.Fatalf("ParseAgyModelsJSON failed: %v", err)
	}
	want := []store.AgentModelItem{
		{ID: "gemini-4-neon-medium", DisplayName: "Gemini 4 Neon (Medium)", Family: quota.ModelFamilyGemini},
		{ID: "claude-opus-5-5-high", DisplayName: "Claude Opus 5.5 (High)", Family: quota.ModelFamilyClaude},
		{ID: "gpt-oss-120b-medium", DisplayName: "GPT-OSS 120B (Medium)", Family: quota.ModelFamilyGPT},
		{ID: "custom-local-model", DisplayName: "Custom Local", Family: quota.ModelFamilyUnknown},
		{ID: "no-label-model", DisplayName: "no-label-model", Family: quota.ModelFamilyUnknown},
	}
	if len(items) != len(want) {
		t.Fatalf("got %d items, want %d: %+v", len(items), len(want), items)
	}
	for i, w := range want {
		if items[i] != w {
			t.Errorf("items[%d] = %+v, want %+v", i, items[i], w)
		}
	}

	// Single-model JSON catalog.
	single, err := ParseAgyModelsJSON([]byte(`{"status":"SUCCESS","command":{"name":"models","data":{"models":[{"id":"new-provider-v1","label":"New Provider V1"}]}}}`))
	if err != nil {
		t.Fatalf("ParseAgyModelsJSON(single) failed: %v", err)
	}
	if len(single) != 1 || single[0].ID != "new-provider-v1" || single[0].DisplayName != "New Provider V1" || single[0].Family != quota.ModelFamilyUnknown {
		t.Fatalf("unexpected single item: %+v", single)
	}

	errCases := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "whitespace only", raw: "  \n\t "},
		{name: "malformed json", raw: `{"status":"SUCCESS"`},
		{name: "combined stderr prefix breaks json", raw: "Fetching available models...\n{\"status\":\"SUCCESS\",\"command\":{\"name\":\"models\",\"data\":{\"models\":[{\"id\":\"a\",\"label\":\"A\"}]}}}"},
		{name: "status error", raw: `{"status":"ERROR","command":{"name":"models","data":{"models":[{"id":"gemini-4-neon-medium","label":"Gemini 4 Neon (Medium)"}]}}}`},
		{name: "status lowercase", raw: `{"status":"success","command":{"name":"models","data":{"models":[{"id":"gemini-4-neon-medium","label":"Gemini 4 Neon (Medium)"}]}}}`},
		{name: "missing command", raw: `{"status":"SUCCESS"}`},
		{name: "wrong command name", raw: `{"status":"SUCCESS","command":{"name":"version","data":{"models":[{"id":"gemini-4-neon-medium","label":"Gemini 4 Neon (Medium)"}]}}}`},
		{name: "missing command data", raw: `{"status":"SUCCESS","command":{"name":"models"}}`},
		{name: "empty models array", raw: `{"status":"SUCCESS","command":{"name":"models","data":{"models":[]}}}`},
		{name: "only blank ids", raw: `{"status":"SUCCESS","command":{"name":"models","data":{"models":[{"id":"  ","label":"Blank"}]}}}`},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ParseAgyModelsJSON([]byte(tc.raw)); err == nil {
				t.Errorf("ParseAgyModelsJSON(%s) succeeded with %+v, want error", tc.name, got)
			}
		})
	}
}

func TestParseAgyModelsTSV(t *testing.T) {
	raw := "gemini-4-neon-medium\tGemini 4 Neon (Medium)\n" +
		"gemini-pro-orchestrator\tOrchestrator (Carbon)\r\n" +
		"\n" +
		"claude-opus-5-5-high\tClaude Opus 5.5 (High)\n" +
		"gpt-oss-120b-medium\tGPT-OSS 120B (Medium)\n" +
		"new-provider-v1\tNew Provider  V1\n" +
		"gemini-4-neon-medium\tDuplicate Ignored\n"

	items, err := ParseAgyModelsTSV(raw)
	if err != nil {
		t.Fatalf("ParseAgyModelsTSV failed: %v", err)
	}
	want := []store.AgentModelItem{
		{ID: "gemini-4-neon-medium", DisplayName: "Gemini 4 Neon (Medium)", Family: quota.ModelFamilyGemini},
		{ID: "gemini-pro-orchestrator", DisplayName: "Orchestrator (Carbon)", Family: quota.ModelFamilyGemini},
		{ID: "claude-opus-5-5-high", DisplayName: "Claude Opus 5.5 (High)", Family: quota.ModelFamilyClaude},
		{ID: "gpt-oss-120b-medium", DisplayName: "GPT-OSS 120B (Medium)", Family: quota.ModelFamilyGPT},
		{ID: "new-provider-v1", DisplayName: "New Provider  V1", Family: quota.ModelFamilyUnknown},
	}
	if len(items) != len(want) {
		t.Fatalf("got %d items, want %d: %+v", len(items), len(want), items)
	}
	for i, w := range want {
		if items[i] != w {
			t.Errorf("items[%d] = %+v, want %+v", i, items[i], w)
		}
	}

	// Single-model TSV catalog.
	single, err := ParseAgyModelsTSV("gemini-4-neon-medium\tGemini 4 Neon (Medium)\n")
	if err != nil {
		t.Fatalf("ParseAgyModelsTSV(single) failed: %v", err)
	}
	if len(single) != 1 || single[0].ID != "gemini-4-neon-medium" {
		t.Fatalf("unexpected single TSV item: %+v", single)
	}

	errCases := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "whitespace only", raw: "\n  \r\n"},
		{name: "prose without tab", raw: "No models available\n"},
		{name: "space padded without tab", raw: "gemini-4-neon-medium   Gemini 4 Neon (Medium)\n"},
		{name: "three tab columns", raw: "gemini-4-neon-medium\tGemini 4 Neon\textra\n"},
		{name: "empty id", raw: "\tGemini 4 Neon (Medium)\n"},
		{name: "empty label", raw: "gemini-4-neon-medium\t   \n"},
		{name: "valid line mixed with non-tsv line", raw: "gemini-4-neon-medium\tGemini 4 Neon (Medium)\nWarning: incomplete\n"},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ParseAgyModelsTSV(tc.raw); err == nil {
				t.Errorf("ParseAgyModelsTSV(%s) succeeded with %+v, want error", tc.name, got)
			}
		})
	}
}

func TestValidateAgyTSVStderr(t *testing.T) {
	okCases := []struct {
		name   string
		stderr string
	}{
		{name: "empty", stderr: ""},
		{name: "whitespace only", stderr: " \n\r\n\t "},
		{name: "known non-tty progress", stderr: "Fetching available models...\n"},
		{name: "crlf progress", stderr: "Fetching available models...\r\n"},
		{name: "spinner and ansi progress", stderr: "\r⠋ Fetching available models...\r\x1b[KFetching available models...\n"},
	}
	for _, tc := range okCases {
		t.Run("ok/"+tc.name, func(t *testing.T) {
			if err := ValidateAgyTSVStderr(tc.stderr); err != nil {
				t.Errorf("ValidateAgyTSVStderr(%q) = %v, want nil", tc.stderr, err)
			}
		})
	}

	errCases := []struct {
		name   string
		stderr string
	}{
		{name: "interrupted", stderr: "Catalog retrieval interrupted\n"},
		{name: "progress then interrupted", stderr: "Fetching available models...\nCatalog retrieval interrupted\n"},
		{name: "progress with trailing failure", stderr: "Fetching available models... failed\n"},
		{name: "warning", stderr: "Warning: partial list\n"},
		{name: "non-ascii error", stderr: "エラー: モデル一覧の取得に失敗しました\n"},
		{name: "cr overwritten diagnostic", stderr: "Catalog retrieval interrupted\rFetching available models...\n"},
	}
	for _, tc := range errCases {
		t.Run("err/"+tc.name, func(t *testing.T) {
			if err := ValidateAgyTSVStderr(tc.stderr); err == nil {
				t.Errorf("ValidateAgyTSVStderr(%q) = nil, want error", tc.stderr)
			}
		})
	}
}

func TestProberAgyJSONPrimaryAndSeparatedStreams(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), store.Options{InstanceID: "agy-json-primary"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		InstanceID: "agy-json-primary",
		Hosts: map[string]config.HostConfig{
			"ubuntu": {
				OS: "linux",
				Agents: map[string]config.HostAgentOverride{
					"agy": {},
				},
			},
		},
	}

	var cmds []string
	jsonStdout := `{"status":"SUCCESS","command":{"name":"models","data":{"models":[{"id":"gemini-4-neon-medium","label":"Gemini 4 Neon (Medium)"},{"id":"claude-opus-5-5-high","label":"Claude Opus 5.5 (High)"}]}}}`
	provider := &fakeHostProvider{
		hosts: map[string]adapter.FileAccess{
			"ubuntu": &fakeFileAccess{
				runFn: func(ctx context.Context, cmd string) (adapter.ExecResult, error) {
					cmds = append(cmds, cmd)
					return adapter.ExecResult{
						ExitCode: 0,
						Stdout:   jsonStdout,
						Stderr:   "Fetching available models...\n",
						// Combined Output intentionally corrupted with stderr prefix to prove
						// the prober reads Stdout strictly rather than Output.
						Output: "Fetching available models...\n" + jsonStdout,
					}, nil
				},
			},
		},
	}

	t1 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	prober, err := NewProber(ProberOptions{
		Store:    st,
		Config:   cfg,
		Provider: provider,
		Now:      func() time.Time { return t1 },
		Logger:   log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewProber: %v", err)
	}

	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	if len(cmds) != 1 {
		t.Fatalf("expected 1 command invocation (no fallback), got %d: %v", len(cmds), cmds)
	}
	if !strings.Contains(cmds[0], "--output-format json models") {
		t.Errorf("command = %q, want global flag before subcommand (--output-format json models)", cmds[0])
	}

	got, err := st.GetAgentModels(ctx, "ubuntu", "agy")
	if err != nil {
		t.Fatalf("GetAgentModels: %v", err)
	}
	if got.Status != store.AgentModelStatusOK || got.Source != SourceAgyCLI || len(got.Models) != 2 {
		t.Fatalf("unexpected catalog: %+v", got)
	}
	if got.Models[0].ID != "gemini-4-neon-medium" || got.Models[0].DisplayName != "Gemini 4 Neon (Medium)" || got.Models[0].Family != quota.ModelFamilyGemini {
		t.Errorf("model[0] = %+v", got.Models[0])
	}
	if got.Models[1].ID != "claude-opus-5-5-high" || got.Models[1].DisplayName != "Claude Opus 5.5 (High)" || got.Models[1].Family != quota.ModelFamilyClaude {
		t.Errorf("model[1] = %+v", got.Models[1])
	}
}

func TestProberAgyFallsBackToTSVWhenJSONFails(t *testing.T) {
	cases := []struct {
		name    string
		jsonRes adapter.ExecResult
		jsonErr error
	}{
		{
			name:    "json exits non-zero (unsupported flag)",
			jsonRes: adapter.ExecResult{ExitCode: 2, Stderr: "unknown flag: --output-format\n"},
		},
		{
			name:    "json returns invalid json",
			jsonRes: adapter.ExecResult{ExitCode: 0, Stdout: "not-json\n", Stderr: "Fetching available models...\n"},
		},
		{
			name:    "json status is not SUCCESS",
			jsonRes: adapter.ExecResult{ExitCode: 0, Stdout: `{"status":"ERROR","command":{"name":"models","data":{"models":[]}}}`},
		},
		{
			name:    "json command.name is not models",
			jsonRes: adapter.ExecResult{ExitCode: 0, Stdout: `{"status":"SUCCESS","command":{"name":"other","data":{"models":[{"id":"a","label":"A"}]}}}`},
		},
		{
			name:    "json stdout empty even if combined Output has json",
			jsonRes: adapter.ExecResult{ExitCode: 0, Stdout: "", Output: `{"status":"SUCCESS","command":{"name":"models","data":{"models":[{"id":"a","label":"A"}]}}}`},
		},
		{
			name:    "json command execution error",
			jsonErr: errors.New("temporary exec failure"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), store.Options{InstanceID: "agy-fallback-test"})
			if err != nil {
				t.Fatalf("store.Open: %v", err)
			}
			defer st.Close()

			cfg := &config.Config{
				InstanceID: "agy-fallback-test",
				Hosts: map[string]config.HostConfig{
					"ubuntu": {
						OS: "linux",
						Agents: map[string]config.HostAgentOverride{
							"agy": {},
						},
					},
				},
			}

			var cmds []string
			provider := &fakeHostProvider{
				hosts: map[string]adapter.FileAccess{
					"ubuntu": &fakeFileAccess{
						runFn: func(ctx context.Context, cmd string) (adapter.ExecResult, error) {
							cmds = append(cmds, cmd)
							if strings.Contains(cmd, "--output-format json models") {
								return tc.jsonRes, tc.jsonErr
							}
							return adapter.ExecResult{
								ExitCode: 0,
								Stdout:   "gemini-4-neon-medium\tGemini 4 Neon (Medium)\nclaude-opus-5-5-high\tClaude Opus 5.5 (High)\n",
								Stderr:   "Fetching available models...\n",
								Output:   "Fetching available models...\ngemini-4-neon-medium\tGemini 4 Neon (Medium)\nclaude-opus-5-5-high\tClaude Opus 5.5 (High)\n",
							}, nil
						},
					},
				},
			}

			var logBuf bytes.Buffer
			t1 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
			prober, err := NewProber(ProberOptions{
				Store:    st,
				Config:   cfg,
				Provider: provider,
				Now:      func() time.Time { return t1 },
				Logger:   log.New(&logBuf, "", 0),
			})
			if err != nil {
				t.Fatalf("NewProber: %v", err)
			}

			if err := prober.Probe(ctx); err != nil {
				t.Fatalf("Probe: %v", err)
			}

			if len(cmds) != 2 {
				t.Fatalf("expected 2 commands (JSON then TSV fallback), got %d: %v", len(cmds), cmds)
			}
			if !strings.Contains(cmds[0], "--output-format json models") {
				t.Errorf("first cmd = %q, want --output-format json models", cmds[0])
			}
			if strings.Contains(cmds[1], "--output-format") || !strings.Contains(cmds[1], "models") {
				t.Errorf("second cmd = %q, want legacy agy models without --output-format", cmds[1])
			}

			got, err := st.GetAgentModels(ctx, "ubuntu", "agy")
			if err != nil {
				t.Fatalf("GetAgentModels: %v", err)
			}
			if got.Status != store.AgentModelStatusOK || got.Source != SourceAgyCLI || len(got.Models) != 2 {
				t.Fatalf("unexpected fallback catalog: %+v", got)
			}
			if got.Models[0].ID != "gemini-4-neon-medium" || got.Models[1].ID != "claude-opus-5-5-high" {
				t.Errorf("unexpected models: %+v", got.Models)
			}
			if !strings.Contains(logBuf.String(), "falling back to TSV") {
				t.Errorf("expected fallback log entry, got %q", logBuf.String())
			}
		})
	}
}

func TestProberAgyPreservesCatalogAndLogsWhenBothJSONAndTSVFail(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), store.Options{InstanceID: "agy-both-fail"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		InstanceID: "agy-both-fail",
		Hosts: map[string]config.HostConfig{
			"ubuntu": {
				OS: "linux",
				Agents: map[string]config.HostAgentOverride{
					"agy": {},
				},
			},
		},
	}

	failBoth := false
	provider := &fakeHostProvider{
		hosts: map[string]adapter.FileAccess{
			"ubuntu": &fakeFileAccess{
				runFn: func(ctx context.Context, cmd string) (adapter.ExecResult, error) {
					if !failBoth && strings.Contains(cmd, "--output-format json models") {
						return adapter.ExecResult{
							ExitCode: 0,
							Stdout:   `{"status":"SUCCESS","command":{"name":"models","data":{"models":[{"id":"gemini-4-neon-medium","label":"Gemini 4 Neon (Medium)"},{"id":"claude-opus-5-5-high","label":"Claude Opus 5.5 (High)"}]}}}`,
							Stderr:   "Fetching available models...\n",
						}, nil
					}
					if strings.Contains(cmd, "--output-format json models") {
						return adapter.ExecResult{ExitCode: 1, Stderr: "Error: Please sign in\n"}, nil
					}
					return adapter.ExecResult{ExitCode: 0, Stdout: "No models available\n", Stderr: "Fetching available models...\n"}, nil
				},
			},
		},
	}

	var logBuf bytes.Buffer
	t1 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	now := t1
	prober, err := NewProber(ProberOptions{
		Store:    st,
		Config:   cfg,
		Provider: provider,
		Now:      func() time.Time { return now },
		Logger:   log.New(&logBuf, "", 0),
	})
	if err != nil {
		t.Fatalf("NewProber: %v", err)
	}

	// First probe seeds the catalog via JSON.
	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("first Probe: %v", err)
	}

	// Second probe fails in both JSON and TSV fallback.
	failBoth = true
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
	if !strings.Contains(got.ErrorDetail, "agy json probe failed") || !strings.Contains(got.ErrorDetail, "tsv fallback failed") {
		t.Errorf("error_detail = %q, want both json and tsv failure details", got.ErrorDetail)
	}
	if len(got.Models) != 2 || got.Models[0].ID != "gemini-4-neon-medium" || got.Models[1].ID != "claude-opus-5-5-high" {
		t.Errorf("previous catalog was NOT preserved: %+v", got.Models)
	}
	if got.UpdatedAt == nil || !got.UpdatedAt.Equal(t1) {
		t.Errorf("updated_at = %v, want preserved %v", got.UpdatedAt, t1)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "agy probe failed for ubuntu/agy") {
		t.Errorf("expected explicit failure log for ubuntu/agy, got %q", logged)
	}
}

func TestProberAgyTSVRejectsUnexpectedStderrAndPreservesCatalog(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), store.Options{InstanceID: "agy-tsv-stderr"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		InstanceID: "agy-tsv-stderr",
		Hosts: map[string]config.HostConfig{
			"ubuntu": {
				OS: "linux",
				Agents: map[string]config.HostAgentOverride{
					"agy": {},
				},
			},
		},
	}

	interrupted := false
	provider := &fakeHostProvider{
		hosts: map[string]adapter.FileAccess{
			"ubuntu": &fakeFileAccess{
				runFn: func(ctx context.Context, cmd string) (adapter.ExecResult, error) {
					if !interrupted && strings.Contains(cmd, "--output-format json models") {
						return adapter.ExecResult{
							ExitCode: 0,
							Stdout:   `{"status":"SUCCESS","command":{"name":"models","data":{"models":[{"id":"gemini-4-neon-medium","label":"Gemini 4 Neon (Medium)"},{"id":"claude-opus-5-5-high","label":"Claude Opus 5.5 (High)"}]}}}`,
							Stderr:   "Fetching available models...\n",
						}, nil
					}
					if strings.Contains(cmd, "--output-format json models") {
						return adapter.ExecResult{ExitCode: 2, Stderr: "unknown flag: --output-format\n"}, nil
					}
					// Legacy TSV path exits 0 with partial stdout and an interruption diagnostic on stderr.
					return adapter.ExecResult{
						ExitCode: 0,
						Stdout:   "gemini-4-neon-medium\tGemini\n",
						Stderr:   "Fetching available models...\nCatalog retrieval interrupted\n",
					}, nil
				},
			},
		},
	}

	var logBuf bytes.Buffer
	t1 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	now := t1
	prober, err := NewProber(ProberOptions{
		Store:    st,
		Config:   cfg,
		Provider: provider,
		Now:      func() time.Time { return now },
		Logger:   log.New(&logBuf, "", 0),
	})
	if err != nil {
		t.Fatalf("NewProber: %v", err)
	}

	if err := prober.Probe(ctx); err != nil {
		t.Fatalf("first Probe: %v", err)
	}

	interrupted = true
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
	if !strings.Contains(got.ErrorDetail, "Catalog retrieval interrupted") {
		t.Errorf("error_detail = %q, want stderr diagnostic recorded", got.ErrorDetail)
	}
	if len(got.Models) != 2 || got.Models[0].ID != "gemini-4-neon-medium" || got.Models[1].ID != "claude-opus-5-5-high" {
		t.Errorf("previous catalog was NOT preserved: %+v", got.Models)
	}
	if got.UpdatedAt == nil || !got.UpdatedAt.Equal(t1) {
		t.Errorf("updated_at = %v, want preserved %v", got.UpdatedAt, t1)
	}
	if !strings.Contains(logBuf.String(), "Catalog retrieval interrupted") {
		t.Errorf("expected interruption diagnostic in logs, got %q", logBuf.String())
	}
}

func TestProberAgySharesProbeTimeoutAcrossJSONAndTSV(t *testing.T) {
	t.Run("json timeout stops before tsv fallback", func(t *testing.T) {
		ctx := context.Background()
		st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), store.Options{InstanceID: "agy-timeout-stop"})
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		defer st.Close()

		cfg := &config.Config{
			InstanceID: "agy-timeout-stop",
			Hosts: map[string]config.HostConfig{
				"ubuntu": {
					OS: "linux",
					Agents: map[string]config.HostAgentOverride{
						"agy": {},
					},
				},
			},
		}

		var cmds []string
		provider := &fakeHostProvider{
			hosts: map[string]adapter.FileAccess{
				"ubuntu": &fakeFileAccess{
					runFn: func(ctx context.Context, cmd string) (adapter.ExecResult, error) {
						cmds = append(cmds, cmd)
						<-ctx.Done()
						return adapter.ExecResult{}, ctx.Err()
					},
				},
			},
		}

		prober, err := NewProber(ProberOptions{
			Store:        st,
			Config:       cfg,
			Provider:     provider,
			ProbeTimeout: 50 * time.Millisecond,
			Logger:       log.New(io.Discard, "", 0),
		})
		if err != nil {
			t.Fatalf("NewProber: %v", err)
		}

		if err := prober.Probe(ctx); err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if len(cmds) != 1 {
			t.Fatalf("expected TSV fallback not to run after shared timeout expired, got %d commands: %v", len(cmds), cmds)
		}
	})

	t.Run("json and tsv share the same deadline", func(t *testing.T) {
		ctx := context.Background()
		st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), store.Options{InstanceID: "agy-timeout-share"})
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		defer st.Close()

		cfg := &config.Config{
			InstanceID: "agy-timeout-share",
			Hosts: map[string]config.HostConfig{
				"ubuntu": {
					OS: "linux",
					Agents: map[string]config.HostAgentOverride{
						"agy": {},
					},
				},
			},
		}

		var deadlines []time.Time
		provider := &fakeHostProvider{
			hosts: map[string]adapter.FileAccess{
				"ubuntu": &fakeFileAccess{
					runFn: func(ctx context.Context, cmd string) (adapter.ExecResult, error) {
						dl, ok := ctx.Deadline()
						if !ok {
							t.Errorf("expected context deadline for %q", cmd)
						}
						deadlines = append(deadlines, dl)
						if strings.Contains(cmd, "--output-format json models") {
							time.Sleep(15 * time.Millisecond)
							return adapter.ExecResult{ExitCode: 2, Stderr: "unknown flag: --output-format\n"}, nil
						}
						return adapter.ExecResult{
							ExitCode: 0,
							Stdout:   "gemini-4-neon-medium\tGemini 4 Neon (Medium)\n",
							Stderr:   "Fetching available models...\n",
						}, nil
					},
				},
			},
		}

		prober, err := NewProber(ProberOptions{
			Store:        st,
			Config:       cfg,
			Provider:     provider,
			ProbeTimeout: 500 * time.Millisecond,
			Logger:       log.New(io.Discard, "", 0),
		})
		if err != nil {
			t.Fatalf("NewProber: %v", err)
		}

		if err := prober.Probe(ctx); err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if len(deadlines) != 2 {
			t.Fatalf("expected 2 commands, got %d", len(deadlines))
		}
		if !deadlines[0].Equal(deadlines[1]) {
			t.Errorf("JSON deadline (%v) and TSV deadline (%v) must match (shared per-host/agent timeout)", deadlines[0], deadlines[1])
		}
	})
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
						return adapter.ExecResult{ExitCode: 1, Stderr: "network error", Output: "network error"}, nil
					}
					if strings.Contains(cmd, "models_cache.json") {
						return adapter.ExecResult{
							ExitCode: 0,
							Stdout:   `{"models":[{"slug":"gpt-6.2-sol","display_name":"GPT-6.2-Sol","visibility":"list","priority":1}]}`,
							Output:   `{"models":[{"slug":"gpt-6.2-sol","display_name":"GPT-6.2-Sol","visibility":"list","priority":1}]}`,
						}, nil
					}
					if strings.Contains(cmd, "agy") && strings.Contains(cmd, "--output-format json models") {
						return adapter.ExecResult{
							ExitCode: 0,
							Stdout:   `{"status":"SUCCESS","command":{"name":"models","data":{"models":[{"id":"gemini-4-carbon-medium","label":"Gemini 4 Carbon (Medium)"},{"id":"claude-opus-5-5-high","label":"Claude Opus 5.5 (High)"}]}}}`,
							Stderr:   "Fetching available models...\n",
						}, nil
					}
					return adapter.ExecResult{ExitCode: 1, Stderr: "unexpected cmd: " + cmd}, nil
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

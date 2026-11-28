package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/evaluator"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func TestShepherdCooldownDetection(t *testing.T) {
	for _, tc := range []struct {
		name, agent, stderr, report       string
		exit                              int
		unknown, evaluatorFails, cooldown bool
	}{
		{name: "done", agent: "claude", report: "API Error: 429"},
		{name: "exit zero evaluator failure", agent: "codex", stderr: "usage_limit_reached", evaluatorFails: true},
		{name: "downstream", agent: "codex", exit: 1, stderr: "gh: HTTP 429: Too Many Requests (https://api.github.com/graphql)"},
		{name: "test fail", agent: "agy", exit: 1, report: "--- FAIL: expected 429 Too Many Requests"},
		{name: "bare number", agent: "agy", exit: 1, report: "429"},
		{name: "old signature", agent: "codex", exit: 1, stderr: "usage_limit_reached\n" + strings.Repeat("normal output\n", 30)},
		{name: "claude combined tail", agent: "claude", exit: 1, report: "API Error: 429", cooldown: true},
		{name: "codex stderr", agent: "codex", exit: 1, stderr: "usage_limit_reached", report: "other output", cooldown: true},
		{name: "agy", agent: "agy", exit: 1, stderr: "RESOURCE_EXHAUSTED", cooldown: true},
		{name: "claude JSON", agent: "claude", exit: 1, stderr: "{\n  \"type\": \"rate_limit_error\"\n}\n", cooldown: true},
		{name: "codex JSON", agent: "codex", exit: 1, stderr: "{\n  \"code\": \"rate_limit_exceeded\"\n}\n", cooldown: true},
		{name: "agy JSON", agent: "agy", exit: 1, stderr: "{\n  \"status\": \"RESOURCE_EXHAUSTED\"\n}\n", cooldown: true},
		{name: "long final line", agent: "agy", exit: 1, stderr: strings.Repeat("x", 4200) + " RESOURCE_EXHAUSTED\n", cooldown: true},
		{name: "long downstream line", agent: "agy", exit: 1, stderr: "gh: " + strings.Repeat("x", 4200) + " RESOURCE_EXHAUSTED\n"},
		{name: "quoted prose", agent: "agy", exit: 1, stderr: `"RESOURCE_EXHAUSTED"`},
		{name: "custom quoted pattern", agent: "wrapped", exit: 1, stderr: `"wrapper quota blocked"`, cooldown: true},
		{name: "unknown pass", agent: "codex", unknown: true, stderr: "usage_limit_reached"},
		{name: "unknown fail", agent: "codex", unknown: true, evaluatorFails: true, stderr: "usage_limit_reached", cooldown: true},
		{name: "custom exit", agent: "wrapped", exit: 42, cooldown: true},
		{name: "custom pattern", agent: "wrapped", exit: 1, stderr: "wrapper quota blocked", cooldown: true},
		{name: "custom replaces default", agent: "wrapped", exit: 1, stderr: "usage_limit_reached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			yes := true
			cfg := &config.Config{
				Quota:  config.QuotaConfig{GateMode: config.GateModeEnforce, Cooldown: config.CooldownConfig{Enabled: &yes}},
				Pools:  map[string]config.PoolConfig{"p": {AppliesTo: []config.PoolTarget{{Agent: tc.agent}}}},
				Agents: map[string]config.AgentConfig{"wrapped": {RateLimitPatterns: []string{"wrapper quota blocked"}, RateLimitExitCodes: []int{42}}},
			}
			st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), store.Options{Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if _, _, err := st.CreateJob(ctx, store.CreateJobParams{RequestID: "job", Title: "job", Agent: tc.agent}); err != nil {
				t.Fatal(err)
			}
			job, ok, err := st.Claim(ctx, 1)
			if err != nil || !ok {
				t.Fatalf("claim=%t error=%v", ok, err)
			}
			mock := adapter.NewMock(adapter.MockOptions{})
			mock.SetJobBehavior(job.ID, adapter.MockBehavior{Evidence: &adapter.Evidence{FinalReport: tc.report, ErrLogTail: tc.stderr}})
			if _, err := mock.Launch(ctx, job); err != nil {
				t.Fatal(err)
			}
			eval := &scriptedEvaluator{handler: func(int) (evaluator.Result, error) { return evaluator.Result{Passed: !tc.evaluatorFails}, nil }}
			notify := &recordingNotifier{}
			s := NewShepherd(ShepherdOptions{Config: cfg, Store: st, Adapter: mock, Evaluator: eval, Notifier: notify, Logger: discardLogger()})
			result := s.finishExitedJobFrom(ctx, job, adapter.RunState{ExitCode: tc.exit, ExitCodeUnknown: tc.unknown}, "reconciler")
			if result == nil {
				t.Fatal("job did not reach terminal status")
			}
			wantStatus := store.StatusDone
			if tc.exit != 0 || tc.evaluatorFails {
				wantStatus = store.StatusFailed
			}
			if result.Status != wantStatus {
				t.Fatalf("status=%s want %s", result.Status, wantStatus)
			}
			rows, err := st.ListCooldowns(ctx)
			if err != nil || (len(rows) == 1) != tc.cooldown {
				t.Fatalf("cooldowns=%+v err=%v", rows, err)
			}
			if reports := notify.Reports(); (len(reports) == 1) != tc.cooldown {
				t.Fatalf("reports=%+v", reports)
			}
		})
	}
}

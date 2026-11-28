package admission

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func TestCooldownUnmeasuredDetourWithoutUsageFile(t *testing.T) {
	ctx := context.Background()
	enabled := true
	pools := map[string]config.PoolConfig{"p": {Unmeasured: true}, "alt": {Unmeasured: true}}
	cfg := &config.Config{Pools: pools, Quota: config.QuotaConfig{
		GateMode: config.GateModeEnforce, Cooldown: config.CooldownConfig{Enabled: &enabled},
	}}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), store.Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, err := st.CreateJob(ctx, store.CreateJobParams{RequestID: "trigger", Title: "trigger", Pool: "p"}); err != nil {
		t.Fatal(err)
	}
	job, ok, err := st.Claim(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("claim=%t error=%v", ok, err)
	}
	if _, err := st.FailWithCooldown(ctx, job.ID, job.ClaimedBy, "failed", "failed", "quota"); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{quota.FormatV1, quota.FormatLegacy} {
		a := New(Options{Store: st, Pools: pools, UsageFormat: format})
		got, err := a.EvaluateGate("p", []string{"alt"}, a.NewUsage())
		if err != nil || got.Decision != quota.DecisionWouldPass || got.SelectedPool != "alt" {
			t.Errorf("format=%s gate=%+v error=%v", format, got, err)
		}
	}
}

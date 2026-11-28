package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

// Issue #56: on_rate_limit is accepted, validated, stored and returned; the rate-limit
// fields stay out of the response until they are used.
func TestCreateJobOnRateLimit(t *testing.T) {
	now := time.Now()
	url := selectorGateServer(t, config.SelectorModeOff, config.GateModeOff, healthySnap(now), now, nil)

	status, job, body := postJob(t, url, CreateJobRequest{RequestID: "orl-fail", Title: "t", Agent: "codex", OnRateLimit: "fail"})
	if status != http.StatusCreated || job.OnRateLimit != "fail" {
		t.Fatalf("status=%d job=%+v body=%s", status, job, body)
	}
	if got := getJob(t, url, job.ID); got.OnRateLimit != "fail" {
		t.Fatalf("GET on_rate_limit = %q", got.OnRateLimit)
	}

	status, _, body = postJob(t, url, CreateJobRequest{RequestID: "orl-bad", Title: "t", Agent: "codex", OnRateLimit: "sometimes"})
	if status != http.StatusBadRequest || !strings.Contains(body, "on_rate_limit") {
		t.Fatalf("invalid on_rate_limit: status=%d body=%s", status, body)
	}

	resp := doJSON(t, http.MethodPost, url+"/jobs", "", CreateJobRequest{RequestID: "orl-none", Title: "t", Agent: "codex"})
	var raw struct {
		Job map[string]any `json:"job"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	for _, key := range []string{"on_rate_limit", "rate_limit_retries", "retry_after"} {
		if _, ok := raw.Job[key]; ok {
			t.Errorf("unused %s present in response: %v", key, raw.Job)
		}
	}
}

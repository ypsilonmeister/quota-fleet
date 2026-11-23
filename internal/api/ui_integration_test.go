package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/api"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
	"github.com/ypsilonmeister/quota-fleet/internal/ui"
)

func TestUIShellServedWithoutAuthContainsNoDataAndKeepsAPIAuth(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fleet.db")
	st, err := store.Open(ctx, dbPath, store.Options{InstanceID: "secret-instance-xyz"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const (
		secretToken        = "super-secret-api-token-987654"
		secretTitle        = "CONFIDENTIAL-JOB-TITLE-ALPHA"
		secretInstructions = "SECRET-INSTRUCTIONS-DO-NOT-LEAK"
		secretCriteria     = "SECRET-ACCEPTANCE-CRITERIA"
	)

	job, created, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID:          "req-ui-sec-1",
		Title:              secretTitle,
		Agent:              "claude",
		Host:               "local",
		Workdir:            "/srv/secret-repo",
		Instructions:       secretInstructions,
		AcceptanceCriteria: secretCriteria,
	})
	if err != nil || !created {
		t.Fatalf("CreateJob: job=%v created=%v err=%v", job, created, err)
	}

	srv := api.NewServer(st, api.Options{
		InstanceID:  "secret-instance-xyz",
		Token:       secretToken,
		RequireAuth: true,
		Hosts:       []string{"secret-host-name"},
		Agents:      []string{"secret-agent-name"},
	})

	uiPaths := []string{
		"/ui/",
		"/ui/jobs",
		"/ui/jobs/new",
		"/ui/jobs/1",
		"/ui/gate/held",
		"/ui/gate/decisions",
		"/ui/deliveries",
	}

	for _, path := range uiPaths {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("unauthenticated GET %s status = %d, want 200", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != ui.ContentSecurityPolicy {
			t.Errorf("GET %s CSP = %q, want %q", path, got, ui.ContentSecurityPolicy)
		}
		body := rec.Body.String()
		for _, leak := range []string{
			secretToken,
			secretTitle,
			secretInstructions,
			secretCriteria,
			"secret-instance-xyz",
			"secret-host-name",
			"secret-agent-name",
			"/srv/secret-repo",
		} {
			if strings.Contains(body, leak) {
				t.Errorf("GET %s shell HTML leaked %q", path, leak)
			}
		}
	}

	// JSON API endpoints must still require bearer auth.
	apiPaths := []string{
		"/status",
		"/meta",
		"/jobs?status=queued",
		"/jobs/1",
		"/deliveries",
		"/gate/held",
		"/gate/decisions",
	}
	for _, path := range apiPaths {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET %s status = %d, want 401", path, rec.Code)
		}
	}
}

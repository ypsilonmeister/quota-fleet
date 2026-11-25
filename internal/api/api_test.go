package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func newTestServer(t *testing.T, opts Options) (*Server, *store.Store, *httptest.Server) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fleet.db")
	st, err := store.Open(context.Background(), dbPath, store.Options{
		InstanceID: opts.InstanceID,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard, "", 0)
	}
	srv := NewServer(st, opts)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, st, ts
}

func doJSON(t *testing.T, method, url, token string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP %s %s: %v", method, url, err)
	}
	return resp
}

func decodeResp[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode JSON (status=%d): %v", resp.StatusCode, err)
	}
	return out
}

func TestLifecycleCreateGetClaimCompleteAndStatus(t *testing.T) {
	_, _, ts := newTestServer(t, Options{
		InstanceID: "test-inst-1",
		MaxClaimed: 2,
		Token:      "secret-tok",
	})

	// 1. POST /jobs
	resp := doJSON(t, http.MethodPost, ts.URL+"/jobs", "secret-tok", CreateJobRequest{
		RequestID:          "req-life-1",
		Title:              "Build Phase 1-2",
		Agent:              "claude",
		Host:               "ubuntu",
		Workdir:            "/work/quota-fleet",
		TargetRef:          "impl/phase1-core",
		Instructions:       "Implement HTTP API",
		AcceptanceCriteria: "go test ./...",
		Model:              "claude-opus",
		Pool:               "claude",
		AltPools:           []string{"codex"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /jobs status = %d, want 201", resp.StatusCode)
	}
	created := decodeResp[CreateJobResponse](t, resp)
	if !created.Created || created.Job == nil || created.Job.ID == 0 {
		t.Fatalf("unexpected create response: %+v", created)
	}
	if created.Job.Status != "queued" || created.Job.Title != "Build Phase 1-2" {
		t.Errorf("created job = %+v", created.Job)
	}
	if len(created.Job.AltPools) != 1 || created.Job.AltPools[0] != "codex" {
		t.Errorf("alt_pools = %v, want [codex]", created.Job.AltPools)
	}
	jobID := created.Job.ID

	// 2. GET /jobs/{id}
	resp = doJSON(t, http.MethodGet, fmt.Sprintf("%s/jobs/%d", ts.URL, jobID), "secret-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /jobs/%d status = %d, want 200", jobID, resp.StatusCode)
	}
	got := decodeResp[JobDTO](t, resp)
	if got.ID != jobID || got.Title != "Build Phase 1-2" || got.Status != "queued" {
		t.Errorf("GET /jobs/%d = %+v", jobID, got)
	}

	// GET /jobs/{missing} -> 404
	resp = doJSON(t, http.MethodGet, ts.URL+"/jobs/99999", "secret-tok", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /jobs/99999 status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// GET /jobs without status -> 400
	resp = doJSON(t, http.MethodGet, ts.URL+"/jobs", "secret-tok", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET /jobs (no status) status = %d, want 400", resp.StatusCode)
	}
	errResp := decodeResp[ErrorResponse](t, resp)
	if errResp.Error == "" {
		t.Errorf("expected non-empty error string")
	}

	// GET /jobs?status=queued -> contains jobID
	resp = doJSON(t, http.MethodGet, ts.URL+"/jobs?status=queued&limit=10", "secret-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /jobs?status=queued status = %d, want 200", resp.StatusCode)
	}
	listResp := decodeResp[ListJobsResponse](t, resp)
	if len(listResp.Jobs) != 1 || listResp.Jobs[0].ID != jobID {
		t.Fatalf("listResp = %+v, want 1 job id=%d", listResp, jobID)
	}

	// 3. POST /jobs/{id}/claim
	resp = doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/claim", ts.URL, jobID), "secret-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /jobs/%d/claim status = %d, want 200", jobID, resp.StatusCode)
	}
	claimResp := decodeResp[ClaimResponse](t, resp)
	if !claimResp.Claimed || claimResp.Token == "" || claimResp.Job == nil || claimResp.Job.Status != "claimed" {
		t.Fatalf("unexpected claimResp: %+v", claimResp)
	}

	// 4. POST /jobs/{id}/complete
	resp = doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/complete", ts.URL, jobID), "secret-tok", CompleteRequest{
		Token:      claimResp.Token,
		ResultText: "all tests passed",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /jobs/%d/complete status = %d, want 200", jobID, resp.StatusCode)
	}
	compResp := decodeResp[CompleteResponse](t, resp)
	if compResp.Job.Status != "done" || compResp.Job.ResultText != "all tests passed" {
		t.Errorf("compResp.Job = %+v", compResp.Job)
	}

	// 5. GET /status
	resp = doJSON(t, http.MethodGet, ts.URL+"/status", "secret-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /status status = %d, want 200", resp.StatusCode)
	}
	stResp := decodeResp[StatusResponse](t, resp)
	if stResp.InstanceID != "test-inst-1" || stResp.Counts["done"] != 1 || stResp.Total != 1 {
		t.Errorf("GET /status = %+v", stResp)
	}
}

func TestIdempotentCreateAndValidationErrors(t *testing.T) {
	_, _, ts := newTestServer(t, Options{
		InstanceID: "test-idem",
		MaxClaimed: 2,
	})

	// First create -> 201
	resp1 := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "idem-req-1",
		Title:     "Original Title",
	})
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", resp1.StatusCode)
	}
	c1 := decodeResp[CreateJobResponse](t, resp1)

	// Duplicate create with same request_id -> 200 and same job ID/title
	resp2 := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "idem-req-1",
		Title:     "Different Title",
	})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("duplicate create status = %d, want 200", resp2.StatusCode)
	}
	c2 := decodeResp[CreateJobResponse](t, resp2)
	if c2.Created {
		t.Errorf("duplicate create returned created=true, want false")
	}
	if c2.Job.ID != c1.Job.ID || c2.Job.Title != "Original Title" {
		t.Errorf("duplicate job = %+v, want id=%d title=Original Title", c2.Job, c1.Job.ID)
	}

	// Missing title -> 400
	respBad := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "bad-title",
		Title:     "",
	})
	if respBad.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty title status = %d, want 400", respBad.StatusCode)
	}
	_ = respBad.Body.Close()

	// Missing parent -> 400
	missingParent := int64(123456)
	respOrphan := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "orphan-1",
		Title:     "Orphan job",
		ParentID:  &missingParent,
	})
	if respOrphan.StatusCode != http.StatusBadRequest {
		t.Fatalf("orphan parent status = %d, want 400", respOrphan.StatusCode)
	}
	errOrphan := decodeResp[ErrorResponse](t, respOrphan)
	if !strings.Contains(errOrphan.Error, "parent") {
		t.Errorf("orphan error = %q, want mention of parent", errOrphan.Error)
	}
}

func TestCASConflictsAndClaimLimit(t *testing.T) {
	_, _, ts := newTestServer(t, Options{
		InstanceID: "test-cas",
		MaxClaimed: 1,
	})

	r1 := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{RequestID: "cas-1", Title: "Job 1"})
	j1 := decodeResp[CreateJobResponse](t, r1).Job
	r2 := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{RequestID: "cas-2", Title: "Job 2"})
	j2 := decodeResp[CreateJobResponse](t, r2).Job

	// Claim j1 succeeds.
	rc1 := doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/claim", ts.URL, j1.ID), "", nil)
	c1 := decodeResp[ClaimResponse](t, rc1)
	if !c1.Claimed {
		t.Fatalf("expected j1 to be claimed")
	}

	// Claim j2 hits max_claimed=1 -> 200 {claimed: false}.
	rc2 := doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/claim", ts.URL, j2.ID), "", nil)
	if rc2.StatusCode != http.StatusOK {
		t.Fatalf("claim at cap status = %d, want 200", rc2.StatusCode)
	}
	c2 := decodeResp[ClaimResponse](t, rc2)
	if c2.Claimed {
		t.Errorf("expected j2 claim to return claimed=false at capacity")
	}

	// Token mismatch on complete -> 409 Conflict with current_status=claimed.
	rWrongTok := doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/complete", ts.URL, j1.ID), "", CompleteRequest{
		Token:      "wrong-token",
		ResultText: "nope",
	})
	if rWrongTok.StatusCode != http.StatusConflict {
		t.Fatalf("wrong token complete status = %d, want 409", rWrongTok.StatusCode)
	}
	confTok := decodeResp[ConflictResponse](t, rWrongTok)
	if confTok.Error == "" || confTok.CurrentStatus != "claimed" {
		t.Errorf("unexpected token mismatch conflict body: %+v", confTok)
	}

	// Valid complete -> 200.
	rComp := doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/complete", ts.URL, j1.ID), "", CompleteRequest{
		Token:      c1.Token,
		ResultText: "first complete",
	})
	if rComp.StatusCode != http.StatusOK {
		t.Fatalf("first complete status = %d, want 200", rComp.StatusCode)
	}
	_ = rComp.Body.Close()

	// Double complete -> 409 Conflict with current_status=done.
	rDouble := doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/complete", ts.URL, j1.ID), "", CompleteRequest{
		Token:      c1.Token,
		ResultText: "second complete",
	})
	if rDouble.StatusCode != http.StatusConflict {
		t.Fatalf("double complete status = %d, want 409", rDouble.StatusCode)
	}
	confDouble := decodeResp[ConflictResponse](t, rDouble)
	if confDouble.Error == "" || confDouble.CurrentStatus != "done" || confDouble.Job == nil || confDouble.Job.Status != "done" {
		t.Errorf("unexpected double complete conflict body: %+v", confDouble)
	}

	// Exercise remaining transitions: claim j2 -> reset -> claim -> fail -> requeue -> claim -> cancel-claimed, plus reject-invalid & cancel.
	rc2 = doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/claim", ts.URL, j2.ID), "", nil)
	c2 = decodeResp[ClaimResponse](t, rc2)
	if !c2.Claimed {
		t.Fatalf("claim j2 after j1 complete failed")
	}
	rReset := doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/reset", ts.URL, j2.ID), "", nil)
	if rReset.StatusCode != http.StatusOK {
		t.Fatalf("reset status = %d, want 200", rReset.StatusCode)
	}
	_ = rReset.Body.Close()

	rc2 = doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/claim", ts.URL, j2.ID), "", nil)
	c2 = decodeResp[ClaimResponse](t, rc2)
	rFail := doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/fail", ts.URL, j2.ID), "", FailRequest{
		Token:     c2.Token,
		LastError: "boom",
	})
	if rFail.StatusCode != http.StatusOK {
		t.Fatalf("fail status = %d, want 200", rFail.StatusCode)
	}
	_ = rFail.Body.Close()

	rReq := doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/requeue", ts.URL, j2.ID), "", nil)
	if rReq.StatusCode != http.StatusOK {
		t.Fatalf("requeue status = %d, want 200", rReq.StatusCode)
	}
	_ = rReq.Body.Close()

	rc2 = doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/claim", ts.URL, j2.ID), "", nil)
	c2 = decodeResp[ClaimResponse](t, rc2)
	rCC := doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/cancel-claimed", ts.URL, j2.ID), "", FailRequest{
		Token:     c2.Token,
		LastError: "aborted",
	})
	if rCC.StatusCode != http.StatusOK {
		t.Fatalf("cancel-claimed status = %d, want 200", rCC.StatusCode)
	}
	_ = rCC.Body.Close()

	// Reject-invalid & cancel on queued jobs.
	r3 := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{RequestID: "cas-3", Title: "Job 3"})
	j3 := decodeResp[CreateJobResponse](t, r3).Job
	rRej := doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/reject-invalid", ts.URL, j3.ID), "", ReasonRequest{LastError: "bad"})
	if rRej.StatusCode != http.StatusOK {
		t.Fatalf("reject-invalid status = %d, want 200", rRej.StatusCode)
	}
	_ = rRej.Body.Close()

	r4 := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{RequestID: "cas-4", Title: "Job 4"})
	j4 := decodeResp[CreateJobResponse](t, r4).Job
	rCan := doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/cancel", ts.URL, j4.ID), "", ReasonRequest{LastError: "no longer needed"})
	if rCan.StatusCode != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200", rCan.StatusCode)
	}
	_ = rCan.Body.Close()
}

func TestAuthMiddleware(t *testing.T) {
	// Configured token rejects missing and mismatched tokens with 401.
	_, _, ts := newTestServer(t, Options{
		InstanceID: "auth-inst",
		Token:      "top-secret",
	})

	resp := doJSON(t, http.MethodGet, ts.URL+"/status", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", resp.StatusCode)
	}
	errBody := decodeResp[ErrorResponse](t, resp)
	if errBody.Error == "" {
		t.Errorf("401 body missing error field")
	}

	resp = doJSON(t, http.MethodGet, ts.URL+"/status", "wrong-secret", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = doJSON(t, http.MethodGet, ts.URL+"/status", "top-secret", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Empty token with RequireAuth=false logs warning and allows requests.
	var warnBuf bytes.Buffer
	_, _, tsOpen := newTestServer(t, Options{
		InstanceID:  "open-inst",
		Token:       "",
		RequireAuth: false,
		Logger:      log.New(&warnBuf, "", 0),
	})
	if !strings.Contains(warnBuf.String(), "WARNING") {
		t.Errorf("expected startup warning when token is empty, got %q", warnBuf.String())
	}
	resp = doJSON(t, http.MethodGet, tsOpen.URL+"/status", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty token (RequireAuth=false) status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Empty token with RequireAuth=true rejects requests with 401.
	_, _, tsStrict := newTestServer(t, Options{
		InstanceID:  "strict-inst",
		Token:       "",
		RequireAuth: true,
	})
	resp = doJSON(t, http.MethodGet, tsStrict.URL+"/status", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty token (RequireAuth=true) status = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestDeliveriesReturnsOnlyUndeliveredDoneAndFailed(t *testing.T) {
	_, _, ts := newTestServer(t, Options{
		InstanceID: "deliv-inst",
		MaxClaimed: 5,
	})

	create := func(reqID, title string) *JobDTO {
		resp := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{RequestID: reqID, Title: title})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %s status=%d", reqID, resp.StatusCode)
		}
		return decodeResp[CreateJobResponse](t, resp).Job
	}
	claim := func(id int64) string {
		resp := doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/claim", ts.URL, id), "", nil)
		cr := decodeResp[ClaimResponse](t, resp)
		if !cr.Claimed {
			t.Fatalf("claim %d failed", id)
		}
		return cr.Token
	}

	// Job 1: done & delivered -> should NOT appear
	jDoneDelivered := create("d-1", "Done Delivered")
	tok1 := claim(jDoneDelivered.ID)
	_ = doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/complete", ts.URL, jDoneDelivered.ID), "", CompleteRequest{Token: tok1, ResultText: "ok"}).Body.Close()
	rDel1 := doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/deliver", ts.URL, jDoneDelivered.ID), "", nil)
	if rDel1.StatusCode != http.StatusOK {
		t.Fatalf("deliver j1 status=%d", rDel1.StatusCode)
	}
	_ = rDel1.Body.Close()

	// Job 2: done & undelivered -> SHOULD appear
	jDoneUndel := create("d-2", "Done Undelivered")
	tok2 := claim(jDoneUndel.ID)
	_ = doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/complete", ts.URL, jDoneUndel.ID), "", CompleteRequest{Token: tok2, ResultText: "ok2"}).Body.Close()

	// Job 3: failed & undelivered -> SHOULD appear
	jFailedUndel := create("d-3", "Failed Undelivered")
	tok3 := claim(jFailedUndel.ID)
	_ = doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/fail", ts.URL, jFailedUndel.ID), "", FailRequest{Token: tok3, LastError: "err3"}).Body.Close()

	// Job 4: queued -> should NOT appear
	_ = create("d-4", "Still Queued")

	// Job 5: cancelled -> should NOT appear
	jCancelled := create("d-5", "Cancelled")
	_ = doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/cancel", ts.URL, jCancelled.ID), "", nil).Body.Close()

	resp := doJSON(t, http.MethodGet, ts.URL+"/deliveries", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /deliveries status = %d, want 200", resp.StatusCode)
	}
	deliv := decodeResp[DeliveriesResponse](t, resp)
	if len(deliv.Jobs) != 2 {
		t.Fatalf("deliveries count = %d, want 2 (got %+v)", len(deliv.Jobs), deliv.Jobs)
	}
	gotIDs := map[int64]bool{}
	for _, j := range deliv.Jobs {
		gotIDs[j.ID] = true
		if j.DeliveredAt != nil {
			t.Errorf("returned already-delivered job %d", j.ID)
		}
	}
	if !gotIDs[jDoneUndel.ID] || !gotIDs[jFailedUndel.ID] {
		t.Errorf("deliveries IDs = %v, want {%d, %d}", gotIDs, jDoneUndel.ID, jFailedUndel.ID)
	}

	// Deliver the failed job too, then only jDoneUndel remains.
	rDel3 := doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/deliver", ts.URL, jFailedUndel.ID), "", nil)
	if rDel3.StatusCode != http.StatusOK {
		t.Fatalf("deliver failed job status=%d, want 200", rDel3.StatusCode)
	}
	_ = rDel3.Body.Close()

	resp = doJSON(t, http.MethodGet, ts.URL+"/deliveries", "", nil)
	deliv2 := decodeResp[DeliveriesResponse](t, resp)
	if len(deliv2.Jobs) != 1 || deliv2.Jobs[0].ID != jDoneUndel.ID {
		t.Fatalf("deliveries after marking failed = %+v, want only %d", deliv2.Jobs, jDoneUndel.ID)
	}
}

func TestDryRunQuotaGateOnCreateJob(t *testing.T) {
	usagePath := filepath.Join(t.TempDir(), "usage.json")
	usageJSON := `{
		"updatedAt": "2026-11-23T10:29:49+09:00",
		"services": {
			"claude": {
				"remainingPercent": 83,
				"resetAt": "2026-11-26T20:00:00+09:00",
				"subText": "5h 98%",
				"subResetAt": "2026-11-23T14:30:00+09:00"
			},
			"codex": {
				"remainingPercent": 38,
				"resetAt": "2026-11-24T14:51:23+09:00",
				"subText": "5h 70%",
				"subResetAt": "2026-11-23T12:36:25+09:00"
			},
			"antigravity": {
				"remainingPercent": 0,
				"resetAt": "2026-11-28T18:00:17+09:00",
				"subText": "5h 34%",
				"subResetAt": "2026-11-23T11:00:02+09:00",
				"altPercent": 48,
				"altResetAt": "2026-11-26T01:32:24+09:00",
				"altSubText": "5h 0%",
				"altSubResetAt": "2026-11-23T15:30:02+09:00"
			}
		}
	}`
	if err := os.WriteFile(usagePath, []byte(usageJSON), 0o600); err != nil {
		t.Fatalf("WriteFile usage.json: %v", err)
	}

	fixedNow, _ := time.Parse(time.RFC3339, "2026-11-23T10:40:00+09:00")
	pools := map[string]config.PoolConfig{
		"claude":     {Provider: "claude-usage"},
		"codex":      {Provider: "codex-usage"},
		"agy-gemini": {Provider: "agy-usage", AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "gemini"}}},
		"agy-3p":     {Provider: "agy-usage", AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "claude-gpt"}}},
		"agy-argon":  {Unmeasured: true},
	}

	_, st, ts := newTestServer(t, Options{
		InstanceID:      "gate-dryrun-inst",
		MaxClaimed:      2,
		GateMode:        config.GateModeDryRun,
		QuotaUsageFile:  usagePath,
		QuotaStaleAfter: 45 * time.Minute,
		Pools:           pools,
		Now:             func() time.Time { return fixedNow },
	})

	// 1. Healthy primary + alt_pools -> job created normally, selects soonest weekly reset (codex: 11/24 < claude: 11/26).
	resp1 := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "dry-1",
		Title:     "Prefer soonest weekly reset",
		Agent:     "claude",
		Pool:      "claude",
		AltPools:  []string{"codex"},
	})
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("POST /jobs #1 status = %d, want 201", resp1.StatusCode)
	}
	j1 := decodeResp[CreateJobResponse](t, resp1).Job

	// 2. Exhausted pool resolved via applies_to (agy + gemini -> agy-gemini, weekly=0) -> job STILL created (dry-run), records would_hold blocked_by=weekly.
	resp2 := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "dry-2",
		Title:     "Applies-to exhausted job",
		Agent:     "agy",
		Model:     "gemini-2.5-pro",
	})
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("POST /jobs #2 status = %d, want 201", resp2.StatusCode)
	}
	j2 := decodeResp[CreateJobResponse](t, resp2).Job
	if j2.Status != "queued" {
		t.Fatalf("dry-run job #2 status = %q, want queued", j2.Status)
	}

	// 3. Unmeasured pool bypass -> job created, records would_pass with bypassed=true and reason=unmeasured.
	resp3 := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "dry-3",
		Title:     "Unmeasured Argon job",
		Pool:      "agy-argon",
	})
	if resp3.StatusCode != http.StatusCreated {
		t.Fatalf("POST /jobs #3 status = %d, want 201", resp3.StatusCode)
	}
	j3 := decodeResp[CreateJobResponse](t, resp3).Job

	// 4. Exhausted by 5h via applies_to (agy + claude-sonnet -> agy-3p, 5h=0%).
	resp4 := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "dry-4",
		Title:     "5h exhausted job",
		Agent:     "agy",
		Model:     "claude-sonnet-4-5",
	})
	if resp4.StatusCode != http.StatusCreated {
		t.Fatalf("POST /jobs #4 status = %d, want 201", resp4.StatusCode)
	}
	j4 := decodeResp[CreateJobResponse](t, resp4).Job

	// 5. Job without pool/alt_pools/applies_to match -> no gate decision recorded.
	resp5 := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "dry-5",
		Title:     "No pool job",
	})
	if resp5.StatusCode != http.StatusCreated {
		t.Fatalf("POST /jobs #5 status = %d, want 201", resp5.StatusCode)
	}
	_ = resp5.Body.Close()

	// Duplicate request_id retry -> does not record a second decision.
	respDup := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "dry-1",
		Title:     "Prefer soonest weekly reset",
		Pool:      "claude",
	})
	if respDup.StatusCode != http.StatusOK {
		t.Fatalf("duplicate POST /jobs status = %d, want 200", respDup.StatusCode)
	}
	_ = respDup.Body.Close()

	decisions, err := st.ListGateDecisions(context.Background(), 20)
	if err != nil {
		t.Fatalf("ListGateDecisions: %v", err)
	}
	if len(decisions) != 4 {
		t.Fatalf("len(decisions) = %d, want 4 (%+v)", len(decisions), decisions)
	}

	// Newest first: j4, j3, j2, j1.
	if decisions[0].JobID != j4.ID || decisions[0].RequestedPool != "agy-3p" || decisions[0].Decision != "would_hold" || decisions[0].BlockedBy != "5h" {
		t.Errorf("decision[0] = %+v, want job=%d agy-3p would_hold 5h", decisions[0], j4.ID)
	}
	if decisions[1].JobID != j3.ID || decisions[1].RequestedPool != "agy-argon" || decisions[1].Decision != "would_pass" || !decisions[1].Bypassed || decisions[1].Reason != "unmeasured" {
		t.Errorf("decision[1] = %+v, want job=%d agy-argon bypassed", decisions[1], j3.ID)
	}
	if decisions[2].JobID != j2.ID || decisions[2].RequestedPool != "agy-gemini" || decisions[2].Decision != "would_hold" || decisions[2].BlockedBy != "weekly" {
		t.Errorf("decision[2] = %+v, want job=%d agy-gemini would_hold weekly", decisions[2], j2.ID)
	}
	if decisions[3].JobID != j1.ID || decisions[3].Decision != "would_pass" || decisions[3].SelectedPool != "codex" || decisions[3].Bypassed {
		t.Errorf("decision[3] = %+v, want job=%d would_pass selected=codex", decisions[3], j1.ID)
	}
	if decisions[3].MeasuredAt == nil {
		t.Errorf("decision[3].MeasuredAt = nil, want parsed updatedAt")
	}
}

func TestGateModeOffDoesNotRecordDecisions(t *testing.T) {
	_, st, ts := newTestServer(t, Options{
		InstanceID: "gate-off-inst",
		MaxClaimed: 2,
		GateMode:   config.GateModeOff,
	})

	resp := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "off-1",
		Title:     "Gate off job",
		Pool:      "claude",
		AltPools:  []string{"codex"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /jobs status = %d, want 201", resp.StatusCode)
	}
	_ = resp.Body.Close()

	decisions, err := st.ListGateDecisions(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListGateDecisions: %v", err)
	}
	if len(decisions) != 0 {
		t.Fatalf("expected 0 gate_decisions when gate_mode=off, got %d", len(decisions))
	}
}

func TestEnforceQuotaGateHoldIdempotencyBypassUnknownAndCancel(t *testing.T) {
	measured, _ := time.Parse(time.RFC3339, "2026-11-23T10:00:00+09:00")
	freshNow := measured.Add(10 * time.Minute)
	staleNow := measured.Add(50 * time.Minute)
	resetSoon := measured.Add(24 * time.Hour)
	resetLater := measured.Add(72 * time.Hour)

	intPtr := func(v int) *int { return &v }

	currentNow := freshNow
	currentSnap := &quota.Snapshot{
		MeasuredAt: measured,
		Pools: map[string]quota.PoolSnapshot{
			"claude": {
				WeeklyRemainingPercent: 0,
				WeeklyResetAt:          &resetLater,
				ShortRemainingPercent:  intPtr(80),
				MeasuredAt:             measured,
			},
			"codex": {
				WeeklyRemainingPercent: 50,
				WeeklyResetAt:          &resetSoon,
				ShortRemainingPercent:  intPtr(0),
				MeasuredAt:             measured,
			},
			"agy-gemini": {
				WeeklyRemainingPercent: 75,
				WeeklyResetAt:          &resetSoon,
				ShortRemainingPercent:  nil, // provisional unknown (short_window_null)
				MeasuredAt:             measured,
			},
			"agy-3p": {
				WeeklyRemainingPercent: 60,
				WeeklyResetAt:          &resetSoon,
				ShortRemainingPercent:  intPtr(90),
				MeasuredAt:             measured,
			},
		},
	}

	pools := map[string]config.PoolConfig{
		"claude":     {Provider: "claude-usage"},
		"codex":      {Provider: "codex-usage"},
		"agy-gemini": {Provider: "agy-usage", AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "gemini"}}},
		"agy-3p":     {Provider: "agy-usage", AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "claude-gpt"}}},
		"agy-argon":  {Unmeasured: true},
	}

	_, st, ts := newTestServer(t, Options{
		InstanceID:      "gate-enforce-inst",
		MaxClaimed:      2,
		GateMode:        config.GateModeEnforce,
		QuotaStaleAfter: 45 * time.Minute,
		Pools:           pools,
		Now:             func() time.Time { return currentNow },
		SnapshotLoader:  func() (*quota.Snapshot, error) { return currentSnap, nil },
	})

	// 1. All candidate pools exhausted -> 202 Accepted with held_id, persisted in held_jobs, NO job created.
	respHold := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID:          "enf-exhausted-1",
		Title:              "Exhausted Pool Job",
		Agent:              "claude",
		Host:               "ubuntu",
		Workdir:            "/work/demo",
		Instructions:       "do work",
		AcceptanceCriteria: "tests pass",
		Model:              "claude-opus",
		Pool:               "claude",
		AltPools:           []string{"codex"},
	})
	if respHold.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /jobs (exhausted) status = %d, want 202", respHold.StatusCode)
	}
	holdResp := decodeResp[HoldJobResponse](t, respHold)
	if !holdResp.Created || holdResp.HeldID == "" || holdResp.Held == nil || holdResp.Held.Status != "held" {
		t.Fatalf("unexpected HoldJobResponse: %+v", holdResp)
	}
	if holdResp.Held.Payload.Title != "Exhausted Pool Job" || holdResp.Held.Payload.Workdir != "/work/demo" || holdResp.Held.SnapshotSummary == "" {
		t.Errorf("held payload/summary incomplete: %+v", holdResp.Held)
	}
	// Verify no job was created in jobs table.
	if _, err := st.GetJobByRequestID(context.Background(), "enf-exhausted-1"); err != store.ErrNotFound {
		t.Fatalf("expected no job for held request_id, got err=%v", err)
	}

	// 2. Idempotency: resending same request_id returns existing held_id (202, created=false), no duplicate held.
	respHoldDup := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "enf-exhausted-1",
		Title:     "Exhausted Pool Job",
		Pool:      "claude",
		AltPools:  []string{"codex"},
	})
	if respHoldDup.StatusCode != http.StatusAccepted {
		t.Fatalf("duplicate held POST /jobs status = %d, want 202", respHoldDup.StatusCode)
	}
	holdDup := decodeResp[HoldJobResponse](t, respHoldDup)
	if holdDup.Created || holdDup.HeldID != holdResp.HeldID {
		t.Fatalf("duplicate hold = %+v, want same held_id=%s created=false", holdDup, holdResp.HeldID)
	}

	// 3. Unmeasured pool bypasses enforce even when snapshot is stale.
	currentNow = staleNow
	respUnm := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "enf-unmeasured-1",
		Title:     "Argon Bypass Job",
		Pool:      "agy-argon",
	})
	if respUnm.StatusCode != http.StatusCreated {
		t.Fatalf("POST /jobs (unmeasured) status = %d, want 201", respUnm.StatusCode)
	}
	unmJob := decodeResp[CreateJobResponse](t, respUnm)
	if !unmJob.Created || unmJob.Job.Pool != "agy-argon" {
		t.Fatalf("unmeasured job = %+v", unmJob)
	}

	// 4. Stale snapshot holds measured pool (202 Accepted, reason=stale).
	respStale := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "enf-stale-1",
		Title:     "Stale Snapshot Job",
		Agent:     "agy",
		Model:     "claude-sonnet-4-5", // resolves to agy-3p via applies_to
	})
	if respStale.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /jobs (stale) status = %d, want 202", respStale.StatusCode)
	}
	staleHold := decodeResp[HoldJobResponse](t, respStale)
	if !staleHold.Created || staleHold.Held.Pool != "agy-3p" || staleHold.Held.Reason != "stale" {
		t.Fatalf("stale hold = %+v", staleHold.Held)
	}

	// 5. Unknown-only (short_window_null) does NOT hold; creates job and records unknown gate_decision.
	currentNow = freshNow
	respUnk := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "enf-unknown-1",
		Title:     "Unknown Short Window Job",
		Agent:     "agy",
		Model:     "gemini-2.5-pro", // resolves to agy-gemini (short=nil)
	})
	if respUnk.StatusCode != http.StatusCreated {
		t.Fatalf("POST /jobs (unknown-only) status = %d, want 201", respUnk.StatusCode)
	}
	unkJob := decodeResp[CreateJobResponse](t, respUnk)
	if !unkJob.Created || unkJob.Job == nil {
		t.Fatalf("unknown-only job = %+v", unkJob)
	}

	// 6. Idempotency: resending request_id of an ALREADY-EXISTING job returns 200 + existing job even if pool is now exhausted.
	currentNow = staleNow // make all measured pools stale
	respExistingJob := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "enf-unknown-1",
		Title:     "Unknown Short Window Job",
		Pool:      "claude", // exhausted/stale
	})
	if respExistingJob.StatusCode != http.StatusOK {
		t.Fatalf("retry existing job request_id status = %d, want 200", respExistingJob.StatusCode)
	}
	existingJobResp := decodeResp[CreateJobResponse](t, respExistingJob)
	if existingJobResp.Created || existingJobResp.Job.ID != unkJob.Job.ID {
		t.Fatalf("retry existing job = %+v, want id=%d created=false", existingJobResp, unkJob.Job.ID)
	}

	// 7. GET /gate/held returns the 2 active held jobs (exhausted + stale).
	respList := doJSON(t, http.MethodGet, ts.URL+"/gate/held", "", nil)
	if respList.StatusCode != http.StatusOK {
		t.Fatalf("GET /gate/held status = %d, want 200", respList.StatusCode)
	}
	listResp := decodeResp[ListHeldJobsResponse](t, respList)
	if len(listResp.Held) != 2 {
		t.Fatalf("GET /gate/held len = %d, want 2 (%+v)", len(listResp.Held), listResp.Held)
	}

	// 8. DELETE /gate/held/{id} cancels without physical deletion; second delete returns 409 Conflict.
	respDel := doJSON(t, http.MethodDelete, ts.URL+"/gate/held/"+holdResp.HeldID, "", nil)
	if respDel.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /gate/held/%s status = %d, want 200", holdResp.HeldID, respDel.StatusCode)
	}
	delResp := decodeResp[HeldJobActionResponse](t, respDel)
	if delResp.Held.Status != "cancelled" || delResp.Held.CancelledAt == nil {
		t.Fatalf("cancelled held = %+v", delResp.Held)
	}

	respDel2 := doJSON(t, http.MethodDelete, ts.URL+"/gate/held/"+holdResp.HeldID, "", nil)
	if respDel2.StatusCode != http.StatusConflict {
		t.Fatalf("second DELETE /gate/held/%s status = %d, want 409", holdResp.HeldID, respDel2.StatusCode)
	}
	confResp := decodeResp[HeldConflictResponse](t, respDel2)
	if confResp.CurrentStatus != "cancelled" {
		t.Errorf("conflict response = %+v", confResp)
	}

	// Active held list now has 1 entry; ?status=all still has 2 (soft cancel, not deleted).
	respListAfter := doJSON(t, http.MethodGet, ts.URL+"/gate/held", "", nil)
	listAfter := decodeResp[ListHeldJobsResponse](t, respListAfter)
	if len(listAfter.Held) != 1 || listAfter.Held[0].ID != staleHold.HeldID {
		t.Fatalf("active held after delete = %+v", listAfter.Held)
	}
	respListAll := doJSON(t, http.MethodGet, ts.URL+"/gate/held?status=all", "", nil)
	listAll := decodeResp[ListHeldJobsResponse](t, respListAll)
	if len(listAll.Held) != 2 {
		t.Fatalf("all held after delete len = %d, want 2", len(listAll.Held))
	}

	// 9. Verify gate_decisions recorded all 4 unique enforce decisions and distinguishes actual holds.
	decs, err := st.ListGateDecisions(context.Background(), 20)
	if err != nil {
		t.Fatalf("ListGateDecisions: %v", err)
	}
	if len(decs) != 4 {
		t.Fatalf("len(gate_decisions) = %d, want 4 (%+v)", len(decs), decs)
	}
	// Newest first: unknown-only (not held), stale (held), unmeasured (not held), exhausted (held).
	if decs[0].Mode != "enforce" || decs[0].Decision != "unknown" || decs[0].Reason != "short_window_null" || decs[0].Held || decs[0].JobID != unkJob.Job.ID {
		t.Errorf("dec[0] = %+v", decs[0])
	}
	if decs[1].Mode != "enforce" || decs[1].Decision != "unknown" || decs[1].Reason != "stale" || !decs[1].Held || decs[1].HeldID != staleHold.HeldID || decs[1].JobID != 0 {
		t.Errorf("dec[1] = %+v", decs[1])
	}
	if decs[2].Mode != "enforce" || decs[2].Decision != "would_pass" || !decs[2].Bypassed || decs[2].Held || decs[2].JobID != unmJob.Job.ID {
		t.Errorf("dec[2] = %+v", decs[2])
	}
	if decs[3].Mode != "enforce" || decs[3].Decision != "would_hold" || decs[3].BlockedBy != "weekly" || !decs[3].Held || decs[3].HeldID != holdResp.HeldID || decs[3].JobID != 0 {
		t.Errorf("dec[3] = %+v", decs[3])
	}
}

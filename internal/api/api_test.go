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
	"path/filepath"
	"strings"
	"testing"

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

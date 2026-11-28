package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/chain"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func chainBody(reqID string, maxRounds any) map[string]any {
	step := func(instr string) map[string]any {
		return map[string]any{"title": "t", "agent": "codex", "instructions": instr}
	}
	m := map[string]any{
		"request_id": reqID,
		"title":      "chain " + reqID,
		"scope":      "o/r#26",
		"workspace":  map[string]any{"host": "h1", "workdir": "/srv/w"},
		"steps": map[string]any{
			"implement": step("implement"),
			"review":    step("write {{verdict.path}} nonce {{verdict.nonce}}"),
			"fix":       step("fix {{verdict.reviewed_commit}}"),
			"publish":   step("publish {{verdict.reviewed_commit}}"),
		},
	}
	if maxRounds != nil {
		m["max_rounds"] = maxRounds
	}
	return m
}

func newChainAPI(t *testing.T) (*store.Store, string) {
	t.Helper()
	_, st, ts := newTestServer(t, Options{
		InstanceID: "chains",
		Tokens: []config.APITokenConfig{
			{Name: "alice", Token: "tok-alice", Scopes: []string{config.ScopeRegister, config.ScopeRead}},
			{Name: "bob", Token: "tok-bob", Scopes: []string{config.ScopeRegister, config.ScopeRead}},
			{Name: "ops", Token: "tok-ops", Scopes: []string{config.ScopeRead, config.ScopeOperate}},
		},
		Chains: ChainOptions{MaxRoundsCap: 10},
	})
	return st, ts.URL
}

func TestChainsAPI(t *testing.T) {
	st, base := newChainAPI(t)

	resp := doJSON(t, http.MethodPost, base+"/chains", "tok-alice", chainBody("c1", nil))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d", resp.StatusCode)
	}
	created := decodeResp[ChainDetailResponse](t, resp)
	if created.Run.Owner != "alice" || created.Run.MaxRounds != 3 || created.Run.MaxSteps != 7 || len(created.Steps) != 1 ||
		created.Steps[0].JobStatus != "queued" || created.Created == nil || !*created.Created {
		t.Fatalf("created: %+v steps=%+v", created.Run, created.Steps)
	}
	job, err := st.GetJob(context.Background(), *created.Steps[0].JobID)
	if err != nil || job.CreatedBy != "alice" || job.Host != "h1" || job.Workdir != "/srv/w" || job.ChainRunID != created.Run.ID {
		t.Fatalf("first job: %+v %v", job, err)
	}

	// GET /jobs/{id} exposes the chain columns.
	jresp := doJSON(t, http.MethodGet, base+"/jobs/"+itoa(job.ID), "tok-alice", nil)
	jdto := decodeResp[JobDTO](t, jresp)
	if jdto.ChainRunID != created.Run.ID || jdto.ChainSeq != 1 {
		t.Fatalf("job dto chain fields: %+v", jdto)
	}

	// Idempotent registration.
	resp = doJSON(t, http.MethodPost, base+"/chains", "tok-alice", chainBody("c1", nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("repeat create status %d", resp.StatusCode)
	}
	if again := decodeResp[ChainDetailResponse](t, resp); again.Run.ID != created.Run.ID || *again.Created {
		t.Fatalf("repeat create: %+v", again.Run)
	}
	// Same key, other definition or other owner → 409.
	if resp := doJSON(t, http.MethodPost, base+"/chains", "tok-alice", chainBody("c1", 2)); resp.StatusCode != http.StatusConflict {
		t.Fatalf("different definition status %d", resp.StatusCode)
	}
	if resp := doJSON(t, http.MethodPost, base+"/chains", "tok-bob", chainBody("c1", nil)); resp.StatusCode != http.StatusConflict {
		t.Fatalf("different owner status %d", resp.StatusCode)
	}
	// max_rounds: 11 > cap → 400; 10 ok.
	if resp := doJSON(t, http.MethodPost, base+"/chains", "tok-alice", chainBody("c-11", 11)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("max_rounds 11 status %d", resp.StatusCode)
	}
	if resp := doJSON(t, http.MethodPost, base+"/chains", "tok-alice", chainBody("c-10", 10)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("max_rounds 10 status %d", resp.StatusCode)
	}
	// Server-owned fields and deferred start points → 400.
	b := chainBody("c-bad", nil)
	b["steps"].(map[string]any)["fix"].(map[string]any)["parent_id"] = 1
	if resp := doJSON(t, http.MethodPost, base+"/chains", "tok-alice", b); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("parent_id in template status %d", resp.StatusCode)
	}
	b = chainBody("c-bad2", nil)
	b["start"] = "review"
	b["start_after_job_id"] = job.ID
	if resp := doJSON(t, http.MethodPost, base+"/chains", "tok-alice", b); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("start review status %d", resp.StatusCode)
	}
	// Scope: ops has no register.
	if resp := doJSON(t, http.MethodPost, base+"/chains", "tok-ops", chainBody("c-ops", nil)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ops create status %d", resp.StatusCode)
	}

	// List and filter.
	list := decodeResp[ListChainsResponse](t, doJSON(t, http.MethodGet, base+"/chains?status=running&scope=o/r%2326", "tok-alice", nil))
	if len(list.Runs) != 2 {
		t.Fatalf("list: %d runs", len(list.Runs))
	}
	page := decodeResp[ListChainsResponse](t, doJSON(t, http.MethodGet, base+"/chains?limit=1", "tok-alice", nil))
	if len(page.Runs) != 1 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("page: %+v", page)
	}
	if resp := doJSON(t, http.MethodGet, base+"/chains?status=bogus", "tok-alice", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad status filter %d", resp.StatusCode)
	}
	if resp := doJSON(t, http.MethodGet, base+"/chains/nope", "tok-alice", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing run %d", resp.StatusCode)
	}

	// Status section.
	status := decodeResp[StatusResponse](t, doJSON(t, http.MethodGet, base+"/status", "tok-alice", nil))
	if status.Chains == nil || status.Chains.Counts["running"] != 2 {
		t.Fatalf("status chains: %+v", status.Chains)
	}

	// Cancel: bob (not owner, no operate) → 403; alice (owner, no operate) → 200; again → 200.
	cancelURL := base + "/chains/" + created.Run.ID + "/cancel"
	if resp := doJSON(t, http.MethodPost, cancelURL, "tok-bob", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob cancel %d", resp.StatusCode)
	}
	resp = doJSON(t, http.MethodPost, cancelURL, "tok-alice", map[string]string{"reason": "not needed"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner cancel %d", resp.StatusCode)
	}
	cr := decodeResp[CancelChainResponse](t, resp)
	if cr.Run.Status != "cancelled" || len(cr.CancelledJobIDs) != 1 || cr.CancelledJobIDs[0] != job.ID {
		t.Fatalf("cancel: %+v", cr)
	}
	resp = doJSON(t, http.MethodPost, cancelURL, "tok-ops", nil)
	if resp.StatusCode != http.StatusOK || !decodeResp[CancelChainResponse](t, resp).AlreadyCancelled {
		t.Fatalf("operate cancel again %d", resp.StatusCode)
	}
}

func TestPostJobsRejectsChainPrefixAndRequeueOfChainJobs(t *testing.T) {
	st, base := newChainAPI(t)
	resp := doJSON(t, http.MethodPost, base+"/jobs", "tok-alice", map[string]any{"request_id": "chain:abc:1", "title": "t"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("chain: prefix status %d", resp.StatusCode)
	}
	if e := decodeResp[ErrorResponse](t, resp); !strings.Contains(e.Error, "reserved") {
		t.Fatalf("error: %q", e.Error)
	}

	created := decodeResp[ChainDetailResponse](t, doJSON(t, http.MethodPost, base+"/chains", "tok-alice", chainBody("rq", nil)))
	ctx := context.Background()
	job, ok, err := st.ClaimByID(ctx, *created.Steps[0].JobID, 10)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := st.Fail(ctx, job.ID, job.ClaimedBy, "x"); err != nil {
		t.Fatal(err)
	}
	resp = doJSON(t, http.MethodPost, base+"/jobs/"+itoa(job.ID)+"/requeue", "tok-ops", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("requeue chain job %d", resp.StatusCode)
	}
	if e := decodeResp[ConflictResponse](t, resp); !strings.Contains(e.Error, "chain-managed") {
		t.Fatalf("error: %q", e.Error)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// Issue #56: POST /chains accepts on_rate_limit per step; the first step's job carries it
// and the stored definition hands it to later steps (publish stays "" by default).
func TestChainsAPIStepOnRateLimit(t *testing.T) {
	st, base := newChainAPI(t)
	ctx := context.Background()

	b := chainBody("orl", nil)
	steps := b["steps"].(map[string]any)
	steps["implement"].(map[string]any)["on_rate_limit"] = "fail"
	steps["fix"].(map[string]any)["on_rate_limit"] = "fail"
	resp := doJSON(t, http.MethodPost, base+"/chains", "tok-alice", b)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d", resp.StatusCode)
	}
	created := decodeResp[ChainDetailResponse](t, resp)
	jdto := decodeResp[JobDTO](t, doJSON(t, http.MethodGet, base+"/jobs/"+itoa(*created.Steps[0].JobID), "tok-alice", nil))
	if jdto.OnRateLimit != store.OnRateLimitFail {
		t.Fatalf("implement job on_rate_limit = %q", jdto.OnRateLimit)
	}

	run, err := st.GetChainRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	def, err := chain.DecodeStored(run.DefinitionJSON)
	if err != nil {
		t.Fatal(err)
	}
	for step, want := range map[string]string{store.ChainStepFix: store.OnRateLimitFail, store.ChainStepPublish: "", store.ChainStepReview: ""} {
		p, err := chain.BuildStepParams(def, chain.StepInput{Run: run, Step: step, Seq: 2, Round: 1, VerdictNonce: "n", ReviewedCommit: "c"})
		if err != nil {
			t.Fatal(err)
		}
		if p.OnRateLimit != want {
			t.Errorf("%s: on_rate_limit = %q, want %q", step, p.OnRateLimit, want)
		}
	}

	bad := chainBody("orl-bad", nil)
	bad["steps"].(map[string]any)["publish"].(map[string]any)["on_rate_limit"] = "sometimes"
	if resp := doJSON(t, http.MethodPost, base+"/chains", "tok-alice", bad); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid on_rate_limit status %d", resp.StatusCode)
	}
}

package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/admission"
	"github.com/ypsilonmeister/quota-fleet/internal/chain"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

// checkedNotifier records chain reports and can fail them on demand.
type checkedNotifier struct {
	mu      sync.Mutex
	fail    error
	reports []Report
}

func (n *checkedNotifier) Deliver(context.Context, *store.Job) error { return nil }
func (n *checkedNotifier) Report(ctx context.Context, r Report) error {
	return n.ReportChecked(ctx, r)
}
func (n *checkedNotifier) ReportChecked(_ context.Context, r Report) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.fail != nil {
		return n.fail
	}
	n.reports = append(n.reports, r)
	return nil
}
func (n *checkedNotifier) setFail(err error) {
	n.mu.Lock()
	n.fail = err
	n.mu.Unlock()
}
func (n *checkedNotifier) Reports() []Report {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Report(nil), n.reports...)
}

// switchableSnapshot is a usage snapshot source a test can flip between healthy and
// exhausted (one pool "codex").
type switchableSnapshot struct {
	mu        sync.Mutex
	exhausted []bool // consumed one per load; last value sticks
	measured  time.Time
}

func (s *switchableSnapshot) set(seq ...bool) {
	s.mu.Lock()
	s.exhausted = append([]bool(nil), seq...)
	s.mu.Unlock()
}

func (s *switchableSnapshot) load() (*quota.Snapshot, error) {
	s.mu.Lock()
	ex := false
	if len(s.exhausted) > 0 {
		ex = s.exhausted[0]
		if len(s.exhausted) > 1 {
			s.exhausted = s.exhausted[1:]
		}
	}
	s.mu.Unlock()
	reset := s.measured.Add(24 * time.Hour)
	short := 90
	weekly := 80
	if ex {
		weekly = 0
	}
	return &quota.Snapshot{
		MeasuredAt: s.measured,
		Pools: map[string]quota.PoolSnapshot{
			"codex": {WeeklyRemainingPercent: weekly, WeeklyResetAt: &reset, ShortRemainingPercent: &short, MeasuredAt: s.measured},
		},
	}, nil
}

type chainEnv struct {
	t        *testing.T
	ctx      context.Context
	st       *store.Store
	dir      string
	adm      *admission.Admission
	drv      *ChainDriver
	notifier *checkedNotifier
	snap     *switchableSnapshot
	now      time.Time
	gateMode string
	workdir  string
	pools    map[string]config.PoolConfig
}

func newChainEnv(t *testing.T, gateMode string) *chainEnv {
	t.Helper()
	st, dir := openTestStore(t, "chain-test")
	measured := time.Date(2026, 11, 26, 1, 0, 0, 0, time.UTC)
	e := &chainEnv{
		t:        t,
		ctx:      context.Background(),
		st:       st,
		dir:      dir,
		notifier: &checkedNotifier{},
		snap:     &switchableSnapshot{measured: measured},
		now:      measured.Add(10 * time.Minute),
		gateMode: gateMode,
		workdir:  t.TempDir(),
		pools:    map[string]config.PoolConfig{"codex": {Provider: "codex-usage"}},
	}
	e.rebuild()
	return e
}

func (e *chainEnv) nowFn() time.Time { return e.now }

// rebuild (re)creates admission and driver on e.st (also after a reopen).
func (e *chainEnv) rebuild() {
	e.adm = admission.New(admission.Options{
		GateMode:     e.gateMode,
		Pools:        e.pools,
		StaleAfter:   45 * time.Minute,
		Now:          e.nowFn,
		LoadSnapshot: e.snap.load,
		GetJob:       e.st.GetJob,
	})
	e.drv = NewChainDriver(ChainDriverOptions{
		Store:      e.st,
		Admission:  e.adm,
		Notifier:   e.notifier,
		Interval:   time.Second,
		StallAfter: 24 * time.Hour,
		Now:        e.nowFn,
		Logger:     discardLogger(),
	})
}

// reopen closes and reopens the database file (a process restart).
func (e *chainEnv) reopen() {
	e.t.Helper()
	_ = e.st.Close()
	st, err := store.Open(e.ctx, filepath.Join(e.dir, "fleet.db"), store.Options{InstanceID: "chain-test"})
	if err != nil {
		e.t.Fatalf("reopen: %v", err)
	}
	e.t.Cleanup(func() { _ = st.Close() })
	e.st = st
	e.rebuild()
}

func (e *chainEnv) raw() *sql.DB {
	e.t.Helper()
	u := url.URL{Scheme: "file", Path: filepath.Join(e.dir, "fleet.db")}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		e.t.Fatalf("open raw db: %v", err)
	}
	e.t.Cleanup(func() { _ = db.Close() })
	return db
}

func (e *chainEnv) exec(query string, args ...any) {
	e.t.Helper()
	if _, err := e.raw().ExecContext(e.ctx, query, args...); err != nil {
		e.t.Fatalf("exec %q: %v", query, err)
	}
}

func (e *chainEnv) definition(reqID string, maxRounds int, pool string) *chain.Definition {
	e.t.Helper()
	step := func(name, instr string) map[string]any {
		m := map[string]any{"title": name + " r{{round}}", "agent": "codex", "model": "gpt-5", "instructions": instr}
		if pool != "" {
			m["pool"] = pool
		}
		return m
	}
	m := map[string]any{
		"request_id": reqID,
		"title":      "chain " + reqID,
		"scope":      "o/r#26",
		"workspace":  map[string]any{"workdir": e.workdir},
		"steps": map[string]any{
			"implement": step("implement", "implement it"),
			"review":    step("review", "review; write {{verdict.path}} with nonce {{verdict.nonce}} (prev #{{prev.job_id}})"),
			"fix":       step("fix", "fix for {{verdict.reviewed_commit}}"),
			"publish":   step("publish", "publish only if head is {{verdict.reviewed_commit}}"),
		},
	}
	if maxRounds > 0 {
		m["max_rounds"] = maxRounds
	}
	b, _ := json.Marshal(m)
	d, err := chain.ParseDefinition(b, 10)
	if err != nil {
		e.t.Fatalf("ParseDefinition: %v", err)
	}
	return d
}

// createRun registers a run the same way POST /chains does.
func (e *chainEnv) createRun(reqID string, maxRounds int, pool string) *store.ChainRun {
	e.t.Helper()
	d := e.definition(reqID, maxRounds, pool)
	js, hash, err := d.CanonicalJSON()
	if err != nil {
		e.t.Fatal(err)
	}
	runID, _ := chain.NewRunID()
	first, reject, err := chain.PlanStep(e.ctx, e.adm, d, chain.StepInput{
		Run: &store.ChainRun{ID: runID, Scope: d.Scope, Owner: "owner"}, Step: store.ChainStepImplement, Seq: 1,
	})
	if err != nil || reject != nil {
		e.t.Fatalf("PlanStep: %v %v", err, reject)
	}
	run, created, err := e.st.CreateChainRun(e.ctx, store.CreateChainRunParams{
		RequestID: d.RequestID, Owner: "owner", Title: d.Title, Scope: d.Scope,
		DefinitionJSON: js, DefinitionHash: hash, DefinitionVersion: chain.DefinitionVersion,
		MaxRounds: d.MaxRounds, MaxSteps: chain.MaxSteps(d.MaxRounds), RunID: runID,
		FirstStep: store.ChainStepImplement, First: first,
	})
	if err != nil || !created {
		e.t.Fatalf("CreateChainRun: created=%v err=%v", created, err)
	}
	return run
}

func (e *chainEnv) snapshot(runID string) *store.ChainSnapshot {
	e.t.Helper()
	s, err := e.st.LoadChainSnapshot(e.ctx, runID)
	if err != nil {
		e.t.Fatalf("LoadChainSnapshot: %v", err)
	}
	return s
}

func (e *chainEnv) run(runID string) *store.ChainRun {
	e.t.Helper()
	r, err := e.st.GetChainRun(e.ctx, runID)
	if err != nil {
		e.t.Fatalf("GetChainRun: %v", err)
	}
	return r
}

func (e *chainEnv) sweep() {
	e.t.Helper()
	if err := e.drv.SweepOnce(e.ctx); err != nil {
		e.t.Fatalf("SweepOnce: %v", err)
	}
}

func (e *chainEnv) steps(runID string) []*store.ChainStep {
	e.t.Helper()
	s, err := e.st.ListChainSteps(e.ctx, runID)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// claimCurrent claims the current step's job and returns it (with its claim token).
func (e *chainEnv) claimCurrent(runID string) *store.Job {
	e.t.Helper()
	s := e.snapshot(runID)
	if s.Job == nil {
		e.t.Fatalf("current step %+v has no job", s.Step)
	}
	job, ok, err := e.st.ClaimByID(e.ctx, s.Job.ID, 1000)
	if err != nil || !ok {
		e.t.Fatalf("ClaimByID #%d: ok=%v err=%v (status %s)", s.Job.ID, ok, err, s.Job.Status)
	}
	return job
}

// finishCurrent completes the current job; for review steps it stores a captured verdict.
func (e *chainEnv) finishCurrent(runID, verdict string) *store.Job {
	e.t.Helper()
	job := e.claimCurrent(runID)
	var art *store.ChainArtifact
	if verdict != "" {
		st, err := e.st.GetChainStep(e.ctx, runID, job.ChainSeq)
		if err != nil {
			e.t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"schema": chain.VerdictSchema, "run_id": runID, "seq": job.ChainSeq,
			"nonce": st.VerdictNonce, "verdict": verdict, "reviewed_commit": testCommit})
		a := chain.ParseVerdict(raw, runID, job.ChainSeq, st.VerdictNonce)
		if a.Status != store.ChainArtifactCaptured {
			e.t.Fatalf("verdict not captured: %+v", a)
		}
		a.JobID = job.ID
		art = &a
	}
	if _, err := e.st.CompleteWithArtifact(e.ctx, job.ID, job.ClaimedBy, "ok", art); err != nil {
		e.t.Fatalf("CompleteWithArtifact: %v", err)
	}
	return job
}

func (e *chainEnv) failCurrent(runID string) {
	e.t.Helper()
	job := e.claimCurrent(runID)
	if _, err := e.st.Fail(e.ctx, job.ID, job.ClaimedBy, "boom"); err != nil {
		e.t.Fatal(err)
	}
}

func (e *chainEnv) countRows(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.raw().QueryRowContext(e.ctx, query, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func (e *chainEnv) notifications(runID string) []*store.ChainNotification {
	e.t.Helper()
	ns, err := e.st.ListChainNotifications(e.ctx, runID)
	if err != nil {
		e.t.Fatal(err)
	}
	return ns
}

func mustStatus(t *testing.T, run *store.ChainRun, status store.ChainRunStatus, reason string) {
	t.Helper()
	if run.Status != status || run.StopReason != reason {
		t.Fatalf("run status=%s reason=%s (%s), want %s %s", run.Status, run.StopReason, run.StopDetail, status, reason)
	}
}

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

var _ = fmt.Sprintf

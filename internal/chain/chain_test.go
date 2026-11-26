package chain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const (
	tRun   = "11111111-2222-4333-8444-555555555555"
	tNonce = "0123456789abcdef0123456789abcdef"
	tSHA   = "0123456789abcdef0123456789abcdef01234567"
)

func verdictJSON(run string, seq int, nonce, verdict string) string {
	b, _ := json.Marshal(map[string]any{
		"schema": VerdictSchema, "run_id": run, "seq": seq, "nonce": nonce,
		"verdict": verdict, "reviewed_commit": tSHA,
	})
	return string(b)
}

// TestParseVerdict fixes docs/22 §7 / §13-4: only a complete, current-step verdict is captured.
func TestParseVerdict(t *testing.T) {
	good := verdictJSON(tRun, 4, tNonce, "changes_needed")
	cases := []struct {
		name   string
		raw    string
		status string
		reason string
	}{
		{"valid", good, store.ChainArtifactCaptured, ""},
		{"valid ok_to_merge", verdictJSON(tRun, 4, tNonce, "ok_to_merge"), store.ChainArtifactCaptured, ""},
		{"previous round copied (seq and nonce differ)", verdictJSON(tRun, 2, "ffffffffffffffffffffffffffffffff", "ok_to_merge"), store.ChainArtifactInvalid, "seq"},
		{"previous round copied with seq edited", verdictJSON(tRun, 4, "ffffffffffffffffffffffffffffffff", "ok_to_merge"), store.ChainArtifactInvalid, "nonce"},
		{"other run", verdictJSON("other", 4, tNonce, "ok_to_merge"), store.ChainArtifactInvalid, "run_id"},
		{"half written", good[:len(good)/2], store.ChainArtifactInvalid, "not valid JSON"},
		{"duplicate key", strings.Replace(good, `{"nonce"`, `{"verdict":"ok_to_merge","nonce"`, 1), store.ChainArtifactInvalid, "duplicate"},
		{"unknown key", strings.Replace(good, `{`, `{"extra":1,`, 1), store.ChainArtifactInvalid, "unknown key"},
		{"unknown verdict value", verdictJSON(tRun, 4, tNonce, "lgtm"), store.ChainArtifactInvalid, "verdict"},
		{"trailing data", good + `{}`, store.ChainArtifactInvalid, "trailing"},
		{"trailing garbage", good + ` x`, store.ChainArtifactInvalid, "trailing"},
		{"missing key", `{"schema":"qf.review-verdict/1","run_id":"` + tRun + `","seq":4,"nonce":"` + tNonce + `","verdict":"ok_to_merge"}`, store.ChainArtifactInvalid, "missing"},
		{"bad commit", strings.Replace(good, tSHA, "HEAD", 1), store.ChainArtifactInvalid, "reviewed_commit"},
		{"wrong schema", strings.Replace(good, VerdictSchema, "qf.review-verdict/2", 1), store.ChainArtifactInvalid, "schema"},
		{"not an object", `["ok_to_merge"]`, store.ChainArtifactInvalid, "object"},
		{"too large", good + strings.Repeat(" ", MaxVerdictBytes), store.ChainArtifactInvalid, "bytes"},
		{"seq not integer", strings.Replace(good, `"seq":4`, `"seq":4.5`, 1), store.ChainArtifactInvalid, "seq"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := ParseVerdict([]byte(tc.raw), tRun, 4, tNonce)
			if a.Status != tc.status {
				t.Fatalf("status = %s (%s), want %s", a.Status, a.Reason, tc.status)
			}
			if tc.reason != "" && !strings.Contains(a.Reason, tc.reason) {
				t.Fatalf("reason %q does not mention %q", a.Reason, tc.reason)
			}
			if a.RawSHA256 == "" {
				t.Fatal("raw_sha256 must always be set")
			}
		})
	}
	// An empty nonce on the step never matches (a manual or legacy step).
	if a := ParseVerdict([]byte(verdictJSON(tRun, 4, "", "ok_to_merge")), tRun, 4, ""); a.Status != store.ChainArtifactInvalid {
		t.Fatalf("empty nonce captured: %+v", a)
	}
}

func baseDef() map[string]any {
	step := func(extra string) map[string]any {
		return map[string]any{"title": "t", "agent": "codex", "instructions": "do it " + extra}
	}
	return map[string]any{
		"request_id": "r1",
		"title":      "chain",
		"workspace":  map[string]any{"workdir": "/tmp/w"},
		"steps": map[string]any{
			"implement": step(""),
			"review":    step("write {{verdict.path}} with nonce {{verdict.nonce}}"),
			"fix":       step("{{verdict.reviewed_commit}}"),
			"publish":   step("{{verdict.reviewed_commit}} round {{round}}"),
		},
	}
}

func parse(t *testing.T, mut func(m map[string]any)) (*Definition, error) {
	t.Helper()
	m := baseDef()
	if mut != nil {
		mut(m)
	}
	b, _ := json.Marshal(m)
	return ParseDefinition(b, 10)
}

func TestParseDefinition(t *testing.T) {
	d, err := parse(t, nil)
	if err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}
	if d.MaxRounds != 3 || d.Start != StartImplement {
		t.Fatalf("defaults: max_rounds=%d start=%q", d.MaxRounds, d.Start)
	}
	if MaxSteps(d.MaxRounds) != 7 {
		t.Fatalf("MaxSteps(3) = %d", MaxSteps(3))
	}
	// worktree_spec with the shared workdir: every step gets the same workdir and spec.
	ws, err := parse(t, func(m map[string]any) {
		m["workspace"] = map[string]any{"workdir": "/tmp/w", "worktree_spec": map[string]any{"repo_path": "/repo", "branch": "feat/x"}}
	})
	if err != nil {
		t.Fatalf("workdir + worktree_spec rejected: %v", err)
	}
	run := &store.ChainRun{ID: tRun, Owner: "tok"}
	for i, step := range []string{store.ChainStepImplement, store.ChainStepReview, store.ChainStepFix, store.ChainStepPublish} {
		p, err := BuildStepParams(ws, StepInput{Run: run, Step: step, Seq: i + 1, Round: 1, VerdictNonce: tNonce, ReviewedCommit: tSHA})
		if err != nil {
			t.Fatal(err)
		}
		if p.Workdir != "/tmp/w" || p.WorktreeSpec == nil || p.WorktreeSpec.Branch != "feat/x" {
			t.Fatalf("%s: workdir=%q spec=%+v", step, p.Workdir, p.WorktreeSpec)
		}
	}
	_, h1, _ := d.CanonicalJSON()
	d2, _ := parse(t, nil)
	_, h2, _ := d2.CanonicalJSON()
	if h1 != h2 {
		t.Fatal("hash of the same definition differs")
	}

	bad := map[string]func(m map[string]any){
		"max_rounds 11 over cap":      func(m map[string]any) { m["max_rounds"] = 11 },
		"max_rounds negative":         func(m map[string]any) { m["max_rounds"] = -1 },
		"start review deferred":       func(m map[string]any) { m["start"] = "review" },
		"start_after_job_id deferred": func(m map[string]any) { m["start_after_job_id"] = 3 },
		"unknown variable": func(m map[string]any) {
			m["steps"].(map[string]any)["implement"].(map[string]any)["instructions"] = "{{prev.result}}"
		},
		"verdict var outside review": func(m map[string]any) {
			m["steps"].(map[string]any)["implement"].(map[string]any)["instructions"] = "{{verdict.nonce}}"
		},
		"reviewed_commit in review": func(m map[string]any) {
			m["steps"].(map[string]any)["review"].(map[string]any)["instructions"] = "{{verdict.path}} {{verdict.nonce}} {{verdict.reviewed_commit}}"
		},
		"review without nonce": func(m map[string]any) {
			m["steps"].(map[string]any)["review"].(map[string]any)["instructions"] = "{{verdict.path}}"
		},
		"server-owned field request_id": func(m map[string]any) {
			m["steps"].(map[string]any)["fix"].(map[string]any)["request_id"] = "x"
		},
		"server-owned field parent_id": func(m map[string]any) {
			m["steps"].(map[string]any)["fix"].(map[string]any)["parent_id"] = 1
		},
		"server-owned field workdir": func(m map[string]any) {
			m["steps"].(map[string]any)["fix"].(map[string]any)["workdir"] = "/x"
		},
		"missing agent":     func(m map[string]any) { delete(m["steps"].(map[string]any)["fix"].(map[string]any), "agent") },
		"missing publish":   func(m map[string]any) { delete(m["steps"].(map[string]any), "publish") },
		"missing workspace": func(m map[string]any) { delete(m, "workspace") },
		// docs/22 §7.2: without a workdir every step would get its own WorkdirRoot/job-<id>.
		"worktree_spec without workdir": func(m map[string]any) {
			m["workspace"] = map[string]any{"worktree_spec": map[string]any{"repo_path": "/repo", "branch": "feat/x"}}
		},
		"huge instructions": func(m map[string]any) {
			m["steps"].(map[string]any)["fix"].(map[string]any)["instructions"] = strings.Repeat("a", MaxInstructionsBytes+1)
		},
	}
	for name, mut := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := parse(t, mut); !errors.Is(err, ErrInvalidDefinition) {
				t.Fatalf("err = %v, want ErrInvalidDefinition", err)
			}
		})
	}
	if _, err := ParseDefinition([]byte(`{"request_id":"r"} {}`), 10); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("trailing data accepted: %v", err)
	}
}

func TestBuildStepParamsExpandsVariables(t *testing.T) {
	d, err := parse(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := &store.ChainRun{ID: tRun, Scope: "o/r#1", Owner: "tok"}
	p, err := BuildStepParams(d, StepInput{Run: run, Step: store.ChainStepReview, Seq: 4, Round: 2, PrevJobID: 9, VerdictNonce: tNonce})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Instructions, VerdictPath(tRun, 4)) || !strings.Contains(p.Instructions, tNonce) {
		t.Fatalf("review instructions not expanded: %q", p.Instructions)
	}
	if p.Workdir != "/tmp/w" || p.CreatedBy != "tok" || p.Agent != "codex" {
		t.Fatalf("params: %+v", p)
	}
	p, err = BuildStepParams(d, StepInput{Run: run, Step: store.ChainStepPublish, Seq: 5, Round: 2, ReviewedCommit: tSHA})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Instructions, tSHA+" round 2") {
		t.Fatalf("publish instructions not expanded: %q", p.Instructions)
	}
	if VerdictPath(tRun, 4) == VerdictPath(tRun, 2) {
		t.Fatal("verdict paths must differ per seq")
	}
}

// TestDecideRepairTable fixes the branch function for every row of docs/22 §6 (R1–R12).
func TestDecideRepairTable(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	id := func(v int64) *int64 { return &v }
	run := func(round, seq int) *store.ChainRun {
		return &store.ChainRun{ID: tRun, Status: store.ChainRunRunning, CurrentSeq: seq, Revision: 3, Round: round, MaxRounds: 3, MaxSteps: 7}
	}
	step := func(name string, seq int, job *int64, held string) *store.ChainStep {
		return &store.ChainStep{RunID: tRun, Seq: seq, Step: name, JobID: job, HeldID: held, CreatedAt: now.Add(-time.Hour)}
	}
	job := func(st store.Status) *store.Job { return &store.Job{ID: 7, Status: st, LastError: "boom"} }
	art := func(status, verdict string, seq int) *store.ChainArtifact {
		return &store.ChainArtifact{JobID: 7, RunID: tRun, Seq: seq, Status: status, Verdict: verdict}
	}
	released := int64(42)
	cases := []struct {
		name   string
		snap   store.ChainSnapshot
		action store.ChainAction
		next   string
		round  int
		reason string
	}{
		{"R1 queued waits", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, id(7), ""), Job: job(store.StatusQueued)}, store.ChainActionWait, "", 0, ""},
		{"R1 claimed waits", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, id(7), ""), Job: job(store.StatusClaimed)}, store.ChainActionWait, "", 0, ""},
		{"R2 implement done → review", store.ChainSnapshot{Run: run(0, 1), Step: step("implement", 1, id(7), ""), Job: job(store.StatusDone)}, store.ChainActionAdvance, "review", 1, ""},
		{"R2 fix done → review", store.ChainSnapshot{Run: run(2, 3), Step: step("fix", 3, id(7), ""), Job: job(store.StatusDone)}, store.ChainActionAdvance, "review", 2, ""},
		{"R3 ok_to_merge → publish", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, id(7), ""), Job: job(store.StatusDone), Artifact: art("captured", "ok_to_merge", 2)}, store.ChainActionAdvance, "publish", 1, ""},
		{"R3 changes_needed → fix", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, id(7), ""), Job: job(store.StatusDone), Artifact: art("captured", "changes_needed", 2)}, store.ChainActionAdvance, "fix", 2, ""},
		{"R3 changes_needed at max → stop", store.ChainSnapshot{Run: run(3, 6), Step: step("review", 6, id(7), ""), Job: job(store.StatusDone), Artifact: art("captured", "changes_needed", 6)}, store.ChainActionStop, "", 0, store.ChainStopMaxRoundsExceeded},
		{"R4 no artifact", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, id(7), ""), Job: job(store.StatusDone)}, store.ChainActionStop, "", 0, store.ChainStopVerdictUnavailable},
		{"R4 missing", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, id(7), ""), Job: job(store.StatusDone), Artifact: art("missing", "", 2)}, store.ChainActionStop, "", 0, store.ChainStopVerdictUnavailable},
		{"R4 invalid", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, id(7), ""), Job: job(store.StatusDone), Artifact: art("invalid", "", 2)}, store.ChainActionStop, "", 0, store.ChainStopVerdictUnavailable},
		{"R4 unavailable", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, id(7), ""), Job: job(store.StatusDone), Artifact: art("unavailable", "", 2)}, store.ChainActionStop, "", 0, store.ChainStopVerdictUnavailable},
		{"R4 artifact of another seq", store.ChainSnapshot{Run: run(1, 4), Step: step("review", 4, id(7), ""), Job: job(store.StatusDone), Artifact: art("captured", "ok_to_merge", 2)}, store.ChainActionStop, "", 0, store.ChainStopVerdictUnavailable},
		{"R5 publish done", store.ChainSnapshot{Run: run(1, 3), Step: step("publish", 3, id(7), ""), Job: job(store.StatusDone)}, store.ChainActionComplete, "", 0, ""},
		{"R6 failed", store.ChainSnapshot{Run: run(0, 1), Step: step("implement", 1, id(7), ""), Job: job(store.StatusFailed)}, store.ChainActionStop, "", 0, store.ChainStopStepFailed},
		{"R7 cancelled", store.ChainSnapshot{Run: run(0, 1), Step: step("implement", 1, id(7), ""), Job: job(store.StatusCancelled)}, store.ChainActionStop, "", 0, store.ChainStopStepCancelled},
		{"R8 held waits", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, nil, "h1"), Held: &store.HeldJob{ID: "h1", Status: store.HeldStatusHeld}}, store.ChainActionWait, "", 0, ""},
		{"R9 held with permanent error", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, nil, "h1"), Held: &store.HeldJob{ID: "h1", Status: store.HeldStatusHeld, LastError: "parent job not found"}}, store.ChainActionStop, "", 0, store.ChainStopReleaseFailed},
		{"R10 released without link", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, nil, "h1"), Held: &store.HeldJob{ID: "h1", Status: store.HeldStatusReleased, ReleasedJobID: &released}}, store.ChainActionLink, "", 0, ""},
		{"R11 held cancelled", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, nil, "h1"), Held: &store.HeldJob{ID: "h1", Status: store.HeldStatusCancelled}}, store.ChainActionStop, "", 0, store.ChainStopStepCancelled},
		{"R12 no step", store.ChainSnapshot{Run: run(1, 2)}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"R12 neither job nor held", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, nil, "")}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"R12 job row missing", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, id(7), ""), JobMissing: true}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"R12 held row missing", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, nil, "h1"), HeldMissing: true}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"budget guard", store.ChainSnapshot{Run: &store.ChainRun{ID: tRun, Status: store.ChainRunRunning, CurrentSeq: 7, Round: 1, MaxRounds: 3, MaxSteps: 7}, Step: step("fix", 7, id(7), ""), Job: job(store.StatusDone)}, store.ChainActionStop, "", 0, store.ChainStopStepBudgetExceeded},
	}
	// Fixtures above describe well-formed runs: the step's job and a released hold's job
	// belong to the step. The ownership cases below are built explicitly.
	for i := range cases {
		sn := &cases[i].snap
		if sn.Job != nil && sn.Step != nil {
			j := *sn.Job
			j.ChainRunID, j.ChainSeq = tRun, sn.Step.Seq
			sn.Job = &j
		}
		if sn.Held != nil && sn.Step != nil {
			h := *sn.Held
			h.ChainRunID, h.ChainSeq = tRun, sn.Step.Seq
			sn.Held = &h
		}
		if sn.Held != nil && sn.Held.Status == store.HeldStatusReleased && sn.Step != nil {
			sn.Held.RequestID = "chain:r:2"
			sn.ReleasedJob = &store.Job{ID: released, Status: store.StatusQueued, RequestID: "chain:r:2", ChainRunID: tRun, ChainSeq: sn.Step.Seq}
		}
	}
	ownJob := func(runID string, seq int) *store.Job {
		return &store.Job{ID: 7, Status: store.StatusDone, RequestID: "chain:r:2", ChainRunID: runID, ChainSeq: seq}
	}
	// relHeld is a released hold owned by tRun at seq.
	relHeld := func(seq int, jobID *int64) *store.HeldJob {
		return &store.HeldJob{ID: "h1", RequestID: "chain:r:2", Status: store.HeldStatusReleased, ReleasedJobID: jobID, ChainRunID: tRun, ChainSeq: seq}
	}
	foreignHeld := func(runID string, seq int, st store.HeldStatus, jobID *int64) *store.HeldJob {
		return &store.HeldJob{ID: "h1", RequestID: "chain:r:2", Status: st, ReleasedJobID: jobID, ChainRunID: runID, ChainSeq: seq}
	}
	cases = append(cases, []struct {
		name   string
		snap   store.ChainSnapshot
		action store.ChainAction
		next   string
		round  int
		reason string
	}{
		{"ownership: R10 unlinked job is linkable", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, nil, "h1"), Held: relHeld(2, &released), ReleasedJob: &store.Job{ID: released, RequestID: "chain:r:2", Status: store.StatusQueued}}, store.ChainActionLink, "", 0, ""},
		{"ownership: R10 job of another run", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, nil, "h1"), Held: relHeld(2, &released), ReleasedJob: &store.Job{ID: released, RequestID: "chain:r:2", Status: store.StatusDone, ChainRunID: "other", ChainSeq: 2}}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"ownership: R10 job of another seq", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, nil, "h1"), Held: relHeld(2, &released), ReleasedJob: &store.Job{ID: released, RequestID: "chain:r:2", Status: store.StatusDone, ChainRunID: tRun, ChainSeq: 1}}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"ownership: R10 job with another request_id", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, nil, "h1"), Held: relHeld(2, &released), ReleasedJob: &store.Job{ID: released, RequestID: "x", Status: store.StatusDone}}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"ownership: R10 released job missing", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, nil, "h1"), Held: relHeld(2, &released), ReleasedJobMissing: true}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"ownership: step job of another run", store.ChainSnapshot{Run: run(0, 1), Step: step("implement", 1, id(7), ""), Job: ownJob("other", 1)}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"ownership: job and hold agree", store.ChainSnapshot{Run: run(0, 1), Step: step("implement", 1, id(7), "h1"), Job: ownJob(tRun, 1), Held: relHeld(1, id(7))}, store.ChainActionAdvance, "review", 1, ""},
		{"ownership: hold points at another job", store.ChainSnapshot{Run: run(0, 1), Step: step("implement", 1, id(7), "h1"), Job: ownJob(tRun, 1), Held: relHeld(1, &released)}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"ownership: hold of a linked step not released", store.ChainSnapshot{Run: run(0, 1), Step: step("implement", 1, id(7), "h1"), Job: ownJob(tRun, 1), Held: &store.HeldJob{ID: "h1", Status: store.HeldStatusHeld, ChainRunID: tRun, ChainSeq: 1}}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"ownership: held hold of another run", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, nil, "h1"), Held: foreignHeld("other", 2, store.HeldStatusHeld, nil)}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"ownership: held hold of another seq", store.ChainSnapshot{Run: run(1, 4), Step: step("review", 4, nil, "h1"), Held: foreignHeld(tRun, 2, store.HeldStatusHeld, nil)}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"ownership: unowned hold", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, nil, "h1"), Held: foreignHeld("", 0, store.HeldStatusHeld, nil)}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"ownership: R10 through another run's released hold", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, nil, "h1"), Held: foreignHeld("other", 2, store.HeldStatusReleased, &released), ReleasedJob: &store.Job{ID: released, RequestID: "chain:r:2", Status: store.StatusQueued}}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"ownership: cancelled hold of another run", store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, nil, "h1"), Held: foreignHeld("other", 2, store.HeldStatusCancelled, nil)}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
		{"ownership: hold of a linked step missing", store.ChainSnapshot{Run: run(0, 1), Step: step("implement", 1, id(7), "h1"), Job: ownJob(tRun, 1), HeldMissing: true}, store.ChainActionStop, "", 0, store.ChainStopInternalInconsistency},
	}...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.snap, now, 24*time.Hour)
			if d.Action != tc.action || d.NextStep != tc.next || (tc.action == store.ChainActionAdvance && d.NextRound != tc.round) || d.StopReason != tc.reason {
				t.Fatalf("Decide = %+v, want action=%s next=%s round=%d reason=%s", d, tc.action, tc.next, tc.round, tc.reason)
			}
			if again := Decide(tc.snap, now.Add(48*time.Hour), time.Hour); again.Key() != d.Key() {
				t.Fatalf("Key depends on time: %s vs %s", again.Key(), d.Key())
			}
		})
	}
	// Stall is reported only after stall_after.
	queued := job(store.StatusQueued)
	queued.ChainRunID, queued.ChainSeq = tRun, 2
	s := store.ChainSnapshot{Run: run(1, 2), Step: step("review", 2, id(7), ""), Job: queued}
	if Decide(s, now, 2*time.Hour).Stalled {
		t.Fatal("stalled before stall_after")
	}
	if !Decide(s, now, 30*time.Minute).Stalled {
		t.Fatal("not stalled after stall_after")
	}
}

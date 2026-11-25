package api

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
)

func registerJob(t *testing.T, url string, req CreateJobRequest, wantStatus int) CreateJobResponse {
	t.Helper()
	resp := doJSON(t, http.MethodPost, url+"/jobs", "", req)
	if resp.StatusCode != wantStatus {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /jobs %s status = %d, want %d: %s", req.RequestID, resp.StatusCode, wantStatus, body)
	}
	return decodeResp[CreateJobResponse](t, resp)
}

func getJob(t *testing.T, url string, id int64) JobDTO {
	t.Helper()
	resp := doJSON(t, http.MethodGet, fmt.Sprintf("%s/jobs/%d", url, id), "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /jobs/%d status = %d", id, resp.StatusCode)
	}
	return decodeResp[JobDTO](t, resp)
}

func TestSupersedeKeyOverHTTP(t *testing.T) {
	for _, mode := range []string{config.GateModeOff, config.GateModeDryRun, config.GateModeEnforce} {
		t.Run(mode, func(t *testing.T) {
			_, _, ts := newTestServer(t, Options{InstanceID: "test-supersede", GateMode: mode})

			old := registerJob(t, ts.URL, CreateJobRequest{RequestID: "r1", Title: "publish", SupersedeKey: "pub"}, 201)
			child := registerJob(t, ts.URL, CreateJobRequest{RequestID: "r1-child", Title: "notify", ParentID: &old.Job.ID}, 201)
			next := registerJob(t, ts.URL, CreateJobRequest{RequestID: "r2", Title: "publish", SupersedeKey: "pub"}, 201)

			check := func(j *JobDTO) {
				t.Helper()
				if j.SupersedeKey != "pub" || !slices.Equal(j.SupersededJobIDs, []int64{old.Job.ID}) ||
					!slices.Equal(j.CancelledChildIDs, []int64{child.Job.ID}) || j.SupersededBy != nil {
					t.Fatalf("new job = %+v supersede = %+v", j, j.SupersedeDTO)
				}
			}
			check(next.Job)
			got := getJob(t, ts.URL, next.Job.ID)
			check(&got)
			// An idempotent replay reports the same relations.
			check(registerJob(t, ts.URL, CreateJobRequest{RequestID: "r2", Title: "publish", SupersedeKey: "pub"}, 200).Job)

			gotOld := getJob(t, ts.URL, old.Job.ID)
			if gotOld.Status != "cancelled" || !strings.HasPrefix(gotOld.LastError, "superseded:") ||
				gotOld.SupersededBy == nil || gotOld.SupersededBy.JobID != next.Job.ID {
				t.Fatalf("old job = %+v by = %+v", gotOld, gotOld.SupersededBy)
			}

			// Depending on the work being replaced is a client error.
			resp := doJSON(t, http.MethodPost, ts.URL+"/jobs", "",
				CreateJobRequest{RequestID: "r3", Title: "publish", SupersedeKey: "pub", ParentID: &next.Job.ID})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("self-superseding chain status = %d, want 400", resp.StatusCode)
			}
			_ = resp.Body.Close()
			if got := getJob(t, ts.URL, next.Job.ID); got.Status != "queued" {
				t.Fatalf("status after rejected registration = %s", got.Status)
			}
		})
	}
}

// Without a supersede_key the responses carry none of the new job fields.
func TestNoSupersedeKeyKeepsResponses(t *testing.T) {
	_, _, ts := newTestServer(t, Options{InstanceID: "test-no-supersede"})
	first := registerJob(t, ts.URL, CreateJobRequest{RequestID: "a", Title: "publish"}, 201)
	second := registerJob(t, ts.URL, CreateJobRequest{RequestID: "b", Title: "publish"}, 201)

	for _, path := range []string{
		fmt.Sprintf("/jobs/%d", first.Job.ID),
		fmt.Sprintf("/jobs/%d", second.Job.ID),
		"/jobs?status=queued",
	} {
		resp := doJSON(t, http.MethodGet, ts.URL+path, "", nil)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, %v", path, resp.StatusCode, err)
		}
		if strings.Contains(string(body), "supersede") {
			t.Errorf("GET %s mentions supersede: %s", path, body)
		}
	}
	if getJob(t, ts.URL, first.Job.ID).Status != "queued" {
		t.Error("unkeyed registration changed an older job")
	}
}

func TestSupersedeHeldListOverHTTP(t *testing.T) {
	now := time.Now().UTC().Add(90 * time.Minute)
	reset := now.Add(24 * time.Hour)
	zero := 0
	_, st, ts := newTestServer(t, Options{
		InstanceID: "test-supersede-held",
		GateMode:   config.GateModeEnforce,
		Pools:      map[string]config.PoolConfig{"claude": {Provider: "anthropic"}},
		Now:        func() time.Time { return now },
		SnapshotLoader: func() (*quota.Snapshot, error) {
			return &quota.Snapshot{MeasuredAt: now, Pools: map[string]quota.PoolSnapshot{
				"claude": {WeeklyRemainingPercent: 0, WeeklyResetAt: &reset, ShortRemainingPercent: &zero, MeasuredAt: now},
			}}, nil
		},
	})
	parent := registerJob(t, ts.URL, CreateJobRequest{RequestID: "parent", Title: "review"}, 201)

	hold := func(requestID, key string, parentID *int64) HoldJobResponse {
		t.Helper()
		resp := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
			RequestID: requestID, Title: "publish", Pool: "claude", SupersedeKey: key, ParentID: parentID,
		})
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("hold %s status = %d, want 202", requestID, resp.StatusCode)
		}
		return decodeResp[HoldJobResponse](t, resp)
	}
	first := hold("r1", "pub", &parent.Job.ID)
	second := hold("r2", "pub", &parent.Job.ID)
	plain := hold("plain", "", nil)

	if second.Held.SupersedeKey != "pub" || !slices.Equal(second.Held.SupersededHeldIDs, []string{first.HeldID}) {
		t.Fatalf("202 response = %+v supersede = %+v", second.Held, second.Held.SupersedeDTO)
	}
	replay := doJSON(t, http.MethodPost, ts.URL+"/jobs", "",
		CreateJobRequest{RequestID: "r1", Title: "publish", Pool: "claude", SupersedeKey: "pub"})
	if replay.StatusCode != http.StatusConflict {
		t.Fatalf("replay superseded hold status = %d, want 409", replay.StatusCode)
	}
	_ = replay.Body.Close()

	list := func(status string) []*HeldJobDTO {
		t.Helper()
		resp := doJSON(t, http.MethodGet, ts.URL+"/gate/held?status="+status, "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /gate/held status = %d", resp.StatusCode)
		}
		return decodeResp[ListHeldJobsResponse](t, resp).Held
	}
	active := list("held")
	if len(active) != 2 || active[0].ID != second.HeldID || active[1].ID != plain.HeldID {
		t.Fatalf("active = %+v", active)
	}
	got := active[0]
	if got.SupersedeKey != "pub" || got.ParentID == nil || *got.ParentID != parent.Job.ID ||
		got.ParentStatus != "queued" || got.CreatedAt.IsZero() ||
		got.AgeSeconds == nil || *got.AgeSeconds < 89*60 || *got.AgeSeconds > 91*60 {
		t.Fatalf("held entry = %+v", got)
	}
	if p := active[1]; p.SupersedeKey != "" || p.ParentID != nil || p.ParentStatus != "" || p.AgeSeconds == nil {
		t.Fatalf("plain entry = %+v", p)
	}
	cancelled := list("cancelled")
	if len(cancelled) != 1 || cancelled[0].ID != first.HeldID ||
		!strings.HasPrefix(cancelled[0].CancelReason, "superseded:") ||
		cancelled[0].SupersededBy == nil || cancelled[0].SupersededBy.HeldID != second.HeldID {
		t.Fatalf("cancelled = %+v", cancelled)
	}

	// A parent that does not exist is reported on the entry.
	if _, err := st.CancelHeldJob(t.Context(), plain.HeldID); err != nil {
		t.Fatal(err)
	}
	missing := int64(999999)
	hold("orphan", "", &missing)
	if active = list("held"); len(active) != 2 || active[1].ParentStatus != "missing" {
		t.Fatalf("orphan entry = %+v", active)
	}
}

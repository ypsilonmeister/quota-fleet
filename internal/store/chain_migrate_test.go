package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOpenRejectsLegacyChainPrefix: the chain: request_id prefix is reserved (docs/22 §4.2);
// an existing database that already uses it outside chain runs is refused on upgrade.
func TestOpenRejectsLegacyChainPrefix(t *testing.T) {
	ctx := context.Background()
	for _, table := range []string{"jobs", "held_jobs"} {
		t.Run(table, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fleet.db")
			st, err := Open(ctx, path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			now := formatTime(time.Now())
			var q string
			if table == "jobs" {
				q = `INSERT INTO jobs (request_id, title, status, created_at, updated_at) VALUES ('chain:legacy:1', 't', 'queued', '` + now + `', '` + now + `')`
			} else {
				q = `INSERT INTO held_jobs (id, request_id, status, pool, payload_json, created_at, updated_at) VALUES ('h', 'chain:legacy:1', 'held', 'p', '{}', '` + now + `', '` + now + `')`
			}
			if _, err := st.writeDB.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
			_ = st.Close()
			if _, err := Open(ctx, path, Options{}); err == nil || !strings.Contains(err.Error(), "reserved request_id prefix") {
				t.Fatalf("Open err = %v", err)
			}
		})
	}
}

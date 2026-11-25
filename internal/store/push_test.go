package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestPushSubscriptionsUpsertListDelete(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleet.db")
	st, err := Open(ctx, path, Options{InstanceID: "push"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	first := PushSubscription{Endpoint: "https://push.example/a", P256dh: "k1", Auth: "a1", UserAgent: "ua1", TokenID: "t1"}
	if created, err := st.UpsertPushSubscription(ctx, first); err != nil || !created {
		t.Fatalf("first upsert = %v, %v", created, err)
	}
	before, err := st.ListPushSubscriptions(ctx)
	if err != nil || len(before) != 1 || before[0].CreatedAt.IsZero() {
		t.Fatalf("list = %+v, %v", before, err)
	}
	renewed := PushSubscription{Endpoint: first.Endpoint, P256dh: "k2", Auth: "a2", UserAgent: "ua2", TokenID: "t2"}
	if created, err := st.UpsertPushSubscription(ctx, renewed); err != nil || created {
		t.Fatalf("second upsert = %v, %v", created, err)
	}

	// The table survives reopening the database.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if st, err = Open(ctx, path, Options{InstanceID: "push"}); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := st.ListPushSubscriptions(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("list = %+v, %v", got, err)
	}
	renewed.CreatedAt = before[0].CreatedAt
	if got[0] != renewed {
		t.Errorf("subscription = %+v, want %+v", got[0], renewed)
	}
	if one, found, err := st.GetPushSubscription(ctx, renewed.Endpoint); err != nil || !found || one != renewed {
		t.Errorf("get = %+v, %v, %v", one, found, err)
	}
	if one, found, err := st.GetPushSubscription(ctx, "https://push.example/other"); err != nil || found {
		t.Errorf("get unknown = %+v, %v, %v", one, found, err)
	}

	// An expiry reported for the replaced keys keeps the renewed subscription.
	if deleted, err := st.DeleteExpiredPushSubscription(ctx, first); err != nil || deleted {
		t.Errorf("expiry of replaced keys = %v, %v", deleted, err)
	}
	if deleted, err := st.DeleteExpiredPushSubscription(ctx, renewed); err != nil || !deleted {
		t.Errorf("expiry = %v, %v", deleted, err)
	}
	if _, err := st.UpsertPushSubscription(ctx, first); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if deleted, err := st.DeletePushSubscription(ctx, "https://push.example/other"); err != nil || deleted {
		t.Errorf("delete unknown = %v, %v", deleted, err)
	}
	if deleted, err := st.DeletePushSubscription(ctx, first.Endpoint); err != nil || !deleted {
		t.Errorf("delete = %v, %v", deleted, err)
	}
	if got, err := st.ListPushSubscriptions(ctx); err != nil || len(got) != 0 {
		t.Errorf("list after delete = %+v, %v", got, err)
	}

	for _, bad := range []PushSubscription{{P256dh: "k", Auth: "a"}, {Endpoint: "https://e", Auth: "a"}, {Endpoint: "https://e", P256dh: "k"}} {
		if _, err := st.UpsertPushSubscription(ctx, bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("upsert %+v = %v, want ErrInvalidInput", bad, err)
		}
	}
}

// A table created before subscriptions were bound to tokens gains the column; its rows
// belong to no token.
func TestPushSubscriptionsGainTokenID(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleet.db")
	st, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, stmt := range []string{
		`DROP TABLE push_subscriptions`,
		`CREATE TABLE push_subscriptions (endpoint TEXT PRIMARY KEY, p256dh TEXT NOT NULL,
			auth TEXT NOT NULL, created_at TEXT NOT NULL, user_agent TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO push_subscriptions VALUES ('https://push.example/old', 'k', 'a', '2026-11-25T00:00:00Z', '')`,
	} {
		if _, err := st.writeDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// The second reopen finds the column in place.
	for range 2 {
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if st, err = Open(ctx, path, Options{}); err != nil {
			t.Fatalf("reopen: %v", err)
		}
	}
	defer st.Close()
	got, err := st.ListPushSubscriptions(ctx)
	if err != nil || len(got) != 1 || got[0].TokenID != "" {
		t.Fatalf("list = %+v, %v", got, err)
	}
	got[0].TokenID = "t"
	if _, err := st.UpsertPushSubscription(ctx, got[0]); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got, err = st.ListPushSubscriptions(ctx); err != nil || len(got) != 1 || got[0].TokenID != "t" {
		t.Fatalf("list = %+v, %v", got, err)
	}
}

func TestJobEventsClosed(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "fleet.db"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if st.JobEventsClosed() {
		t.Fatal("closed after Open")
	}
	st.CloseJobEvents()
	if !st.JobEventsClosed() {
		t.Fatal("not closed after CloseJobEvents")
	}
}

// A deleted endpoint is not registered again, however late the registration arrives, so
// that one overtaken by the removal cannot bring the subscription back (docs/16).
func TestPushDeletedEndpointStaysDeleted(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleet.db")
	st, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	sub := func(endpoint string) PushSubscription {
		return PushSubscription{Endpoint: endpoint, P256dh: "k", Auth: "a", TokenID: "t"}
	}
	registered := sub("https://push.example/registered")
	// The removal of the other one arrives before its first registration does.
	unseen := sub("https://push.example/unseen")
	if _, err := st.UpsertPushSubscription(ctx, registered); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for endpoint, want := range map[string]bool{registered.Endpoint: true, unseen.Endpoint: false} {
		// Deleting twice is harmless.
		for _, existed := range []bool{want, false} {
			if deleted, err := st.DeletePushSubscription(ctx, endpoint); err != nil || deleted != existed {
				t.Fatalf("delete %s = %v, %v, want %v", endpoint, deleted, err, existed)
			}
		}
	}

	// It still holds after a restart.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if st, err = Open(ctx, path, Options{}); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, late := range []PushSubscription{registered, unseen} {
		late.P256dh, late.TokenID = "other-key", "other-token"
		if created, err := st.UpsertPushSubscription(ctx, late); !errors.Is(err, ErrPushEndpointDeleted) || created {
			t.Errorf("late upsert of %s = %v, %v, want ErrPushEndpointDeleted", late.Endpoint, created, err)
		}
	}
	if got, err := st.ListPushSubscriptions(ctx); err != nil || len(got) != 0 {
		t.Errorf("list = %+v, %v", got, err)
	}
	if created, err := st.UpsertPushSubscription(ctx, sub("https://push.example/new")); err != nil || !created {
		t.Errorf("another endpoint = %v, %v", created, err)
	}
}

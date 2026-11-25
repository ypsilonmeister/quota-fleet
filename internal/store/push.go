package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PushSubscription is one browser's Web Push subscription (docs/16).
type PushSubscription struct {
	Endpoint string
	// P256dh and Auth are the base64url keys of the browser's PushSubscription.
	P256dh    string
	Auth      string
	CreatedAt time.Time
	UserAgent string
	// TokenID identifies the API token that registered the subscription. It is empty
	// for a subscription that belongs to no token, which receives nothing.
	TokenID string
}

// ErrPushEndpointDeleted reports a registration of an endpoint that was deleted.
var ErrPushEndpointDeleted = errors.New("push endpoint was deleted; subscribe again for a new one")

// endpointHash identifies a deleted endpoint without keeping its URL, which lets whoever
// knows it send to the browser.
func endpointHash(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:])
}

// UpsertPushSubscription stores sub, keyed by its endpoint. Registering a known endpoint
// again replaces its keys, user agent and token and keeps created_at. created reports whether the
// endpoint was new. sub.CreatedAt is ignored. An endpoint removed with
// DeletePushSubscription is refused with ErrPushEndpointDeleted.
func (s *Store) UpsertPushSubscription(ctx context.Context, sub PushSubscription) (created bool, err error) {
	if strings.TrimSpace(sub.Endpoint) == "" || sub.P256dh == "" || sub.Auth == "" {
		return false, fmt.Errorf("%w: push subscription needs endpoint, p256dh and auth", ErrInvalidInput)
	}
	now := formatTime(time.Now().UTC())
	err = s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var found int
		err := conn.QueryRowContext(ctx, `SELECT 1 FROM push_deleted_endpoints WHERE endpoint_hash = ?`,
			endpointHash(sub.Endpoint)).Scan(&found)
		if err == nil {
			return ErrPushEndpointDeleted
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		err = conn.QueryRowContext(ctx, `SELECT 1 FROM push_subscriptions WHERE endpoint = ?`, sub.Endpoint).Scan(&found)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		created = errors.Is(err, sql.ErrNoRows)
		_, err = conn.ExecContext(ctx, `
			INSERT INTO push_subscriptions (endpoint, p256dh, auth, created_at, user_agent, token_id)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(endpoint) DO UPDATE SET
				p256dh = excluded.p256dh, auth = excluded.auth, user_agent = excluded.user_agent,
				token_id = excluded.token_id
		`, sub.Endpoint, sub.P256dh, sub.Auth, now, sub.UserAgent, sub.TokenID)
		return err
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// DeletePushSubscription removes the subscription for endpoint and reports whether there
// was one. Either way the endpoint is not registered again: a registration sent before
// this call may only arrive after it.
func (s *Store) DeletePushSubscription(ctx context.Context, endpoint string) (bool, error) {
	var deleted bool
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO push_deleted_endpoints (endpoint_hash, deleted_at) VALUES (?, ?)
			ON CONFLICT(endpoint_hash) DO NOTHING
		`, endpointHash(endpoint), formatTime(time.Now().UTC()))
		if err != nil {
			return err
		}
		deleted, err = execDeleted(ctx, conn, `DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint)
		return err
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// DeleteExpiredPushSubscription removes sub after its push service reported it gone. A
// subscription registered again with other keys in the meantime is kept.
func (s *Store) DeleteExpiredPushSubscription(ctx context.Context, sub PushSubscription) (bool, error) {
	var deleted bool
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var err error
		deleted, err = execDeleted(ctx, conn,
			`DELETE FROM push_subscriptions WHERE endpoint = ? AND p256dh = ? AND auth = ?`,
			sub.Endpoint, sub.P256dh, sub.Auth)
		return err
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// execDeleted runs a DELETE and reports whether it removed a row.
func execDeleted(ctx context.Context, conn *sql.Conn, query string, args ...any) (bool, error) {
	res, err := conn.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

const selectPushSubscriptions = `
	SELECT endpoint, p256dh, auth, created_at, user_agent, token_id FROM push_subscriptions`

// GetPushSubscription returns the subscription stored for endpoint; found is false when
// there is none.
func (s *Store) GetPushSubscription(ctx context.Context, endpoint string) (sub PushSubscription, found bool, err error) {
	sub, err = scanPushSubscription(s.readDB.QueryRowContext(ctx,
		selectPushSubscriptions+` WHERE endpoint = ?`, endpoint))
	if errors.Is(err, sql.ErrNoRows) {
		return PushSubscription{}, false, nil
	}
	if err != nil {
		return PushSubscription{}, false, err
	}
	return sub, true, nil
}

// ListPushSubscriptions returns every stored subscription, oldest first.
func (s *Store) ListPushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := s.readDB.QueryContext(ctx, selectPushSubscriptions+` ORDER BY created_at, endpoint`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PushSubscription
	for rows.Next() {
		sub, err := scanPushSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// scanPushSubscription reads one row of selectPushSubscriptions.
func scanPushSubscription(row interface{ Scan(...any) error }) (PushSubscription, error) {
	var (
		sub       PushSubscription
		createdAt string
	)
	if err := row.Scan(&sub.Endpoint, &sub.P256dh, &sub.Auth, &createdAt, &sub.UserAgent, &sub.TokenID); err != nil {
		return PushSubscription{}, err
	}
	var err error
	if sub.CreatedAt, err = parseTime(createdAt); err != nil {
		return PushSubscription{}, fmt.Errorf("parse push subscription created_at: %w", err)
	}
	return sub, nil
}

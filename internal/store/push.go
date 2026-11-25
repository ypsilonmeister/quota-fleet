package store

import (
	"context"
	"database/sql"
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
}

// UpsertPushSubscription stores sub, keyed by its endpoint. Registering a known endpoint
// again replaces its keys and user agent and keeps created_at. created reports whether the
// endpoint was new. sub.CreatedAt is ignored.
func (s *Store) UpsertPushSubscription(ctx context.Context, sub PushSubscription) (created bool, err error) {
	if strings.TrimSpace(sub.Endpoint) == "" || sub.P256dh == "" || sub.Auth == "" {
		return false, fmt.Errorf("%w: push subscription needs endpoint, p256dh and auth", ErrInvalidInput)
	}
	now := formatTime(time.Now().UTC())
	err = s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var found int
		err := conn.QueryRowContext(ctx, `SELECT 1 FROM push_subscriptions WHERE endpoint = ?`, sub.Endpoint).Scan(&found)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		created = errors.Is(err, sql.ErrNoRows)
		_, err = conn.ExecContext(ctx, `
			INSERT INTO push_subscriptions (endpoint, p256dh, auth, created_at, user_agent)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(endpoint) DO UPDATE SET
				p256dh = excluded.p256dh, auth = excluded.auth, user_agent = excluded.user_agent
		`, sub.Endpoint, sub.P256dh, sub.Auth, now, sub.UserAgent)
		return err
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// DeletePushSubscription removes the subscription for endpoint and reports whether there
// was one.
func (s *Store) DeletePushSubscription(ctx context.Context, endpoint string) (bool, error) {
	return s.deletePushSubscriptions(ctx, `DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint)
}

// DeleteExpiredPushSubscription removes sub after its push service reported it gone. A
// subscription registered again with other keys in the meantime is kept.
func (s *Store) DeleteExpiredPushSubscription(ctx context.Context, sub PushSubscription) (bool, error) {
	return s.deletePushSubscriptions(ctx,
		`DELETE FROM push_subscriptions WHERE endpoint = ? AND p256dh = ? AND auth = ?`,
		sub.Endpoint, sub.P256dh, sub.Auth)
}

func (s *Store) deletePushSubscriptions(ctx context.Context, query string, args ...any) (bool, error) {
	var deleted bool
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		deleted = n > 0
		return err
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// ListPushSubscriptions returns every stored subscription, oldest first.
func (s *Store) ListPushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := s.readDB.QueryContext(ctx, `
		SELECT endpoint, p256dh, auth, created_at, user_agent
		FROM push_subscriptions ORDER BY created_at, endpoint
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PushSubscription
	for rows.Next() {
		var (
			sub       PushSubscription
			createdAt string
		)
		if err := rows.Scan(&sub.Endpoint, &sub.P256dh, &sub.Auth, &createdAt, &sub.UserAgent); err != nil {
			return nil, err
		}
		if sub.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, fmt.Errorf("parse push subscription created_at: %w", err)
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

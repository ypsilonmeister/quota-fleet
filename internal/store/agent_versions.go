package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AgentVersionStatus represents the probe outcome for a (host, agent) pair.
type AgentVersionStatus string

const (
	AgentVersionStatusOK       AgentVersionStatus = "ok"
	AgentVersionStatusNotFound AgentVersionStatus = "not_found"
	AgentVersionStatusError    AgentVersionStatus = "error"
)

// Valid reports whether s is a supported AgentVersionStatus.
func (s AgentVersionStatus) Valid() bool {
	switch s {
	case AgentVersionStatusOK, AgentVersionStatusNotFound, AgentVersionStatusError:
		return true
	default:
		return false
	}
}

// AgentVersionEventType represents the category of an agent_version_events record.
type AgentVersionEventType string

const (
	AgentEventVersionChanged AgentVersionEventType = "version_changed"
	AgentEventDriftStarted   AgentVersionEventType = "drift_started"
	AgentEventDriftResolved  AgentVersionEventType = "drift_resolved"
	AgentEventProbeFailed    AgentVersionEventType = "probe_failed"
)

// Valid reports whether t is a supported AgentVersionEventType.
func (t AgentVersionEventType) Valid() bool {
	switch t {
	case AgentEventVersionChanged, AgentEventDriftStarted, AgentEventDriftResolved, AgentEventProbeFailed:
		return true
	default:
		return false
	}
}

// AgentVersion represents the latest probed version state for a (host, agent) pair.
type AgentVersion struct {
	Host        string
	Agent       string
	VersionRaw  string
	Version     string
	Status      AgentVersionStatus
	ErrorDetail string
	CheckedAt   time.Time
}

// AgentVersionEvent represents a single change event recorded in agent_version_events.
type AgentVersionEvent struct {
	ID           int64
	EventType    AgentVersionEventType
	Agent        string
	Host         string
	OldVersion   string
	NewVersion   string
	Detail       string
	ManualUpdate bool
	CreatedAt    time.Time
}

// AgentVersionEventsFilter specifies optional query filters for ListAgentVersionEvents.
type AgentVersionEventsFilter struct {
	Since     *time.Time
	Agent     string
	Host      string
	EventType AgentVersionEventType
	Limit     int
}

// UpsertAgentVersion inserts or updates the latest probed version for (host, agent).
func (s *Store) UpsertAgentVersion(ctx context.Context, v AgentVersion) error {
	host := strings.TrimSpace(v.Host)
	agent := strings.TrimSpace(v.Agent)
	if host == "" || agent == "" {
		return fmt.Errorf("%w: host and agent must not be empty", ErrInvalidInput)
	}
	if !v.Status.Valid() {
		return fmt.Errorf("%w: invalid agent version status %q", ErrInvalidInput, v.Status)
	}
	checkedAt := v.CheckedAt
	if checkedAt.IsZero() {
		checkedAt = time.Now().UTC()
	}

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO agent_versions (host, agent, version_raw, version, status, error_detail, checked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(host, agent) DO UPDATE SET
				version_raw = excluded.version_raw,
				version = excluded.version,
				status = excluded.status,
				error_detail = excluded.error_detail,
				checked_at = excluded.checked_at
		`, host, agent, v.VersionRaw, v.Version, string(v.Status), v.ErrorDetail, formatTime(checkedAt))
		return err
	})
}

// GetAgentVersion fetches the latest stored version for (host, agent).
func (s *Store) GetAgentVersion(ctx context.Context, host, agent string) (*AgentVersion, error) {
	host = strings.TrimSpace(host)
	agent = strings.TrimSpace(agent)
	if host == "" || agent == "" {
		return nil, fmt.Errorf("%w: host and agent must not be empty", ErrInvalidInput)
	}

	row := s.readDB.QueryRowContext(ctx, `
		SELECT host, agent, version_raw, version, status, error_detail, checked_at
		FROM agent_versions
		WHERE host = ? AND agent = ?
	`, host, agent)

	v, err := scanAgentVersion(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return v, nil
}

// ListAgentVersions returns all stored agent_versions rows ordered by host, agent.
func (s *Store) ListAgentVersions(ctx context.Context) ([]*AgentVersion, error) {
	rows, err := s.readDB.QueryContext(ctx, `
		SELECT host, agent, version_raw, version, status, error_detail, checked_at
		FROM agent_versions
		ORDER BY host ASC, agent ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*AgentVersion
	for rows.Next() {
		v, err := scanAgentVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// RecordAgentVersionEvent inserts a new event into agent_version_events.
func (s *Store) RecordAgentVersionEvent(ctx context.Context, ev AgentVersionEvent) (*AgentVersionEvent, error) {
	if !ev.EventType.Valid() {
		return nil, fmt.Errorf("%w: invalid agent version event type %q", ErrInvalidInput, ev.EventType)
	}
	agent := strings.TrimSpace(ev.Agent)
	if agent == "" {
		return nil, fmt.Errorf("%w: agent must not be empty", ErrInvalidInput)
	}
	createdAt := ev.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	manualInt := 0
	if ev.ManualUpdate {
		manualInt = 1
	}

	var recorded *AgentVersionEvent
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			INSERT INTO agent_version_events (event_type, agent, host, old_version, new_version, detail, manual_update, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, string(ev.EventType), agent, strings.TrimSpace(ev.Host), ev.OldVersion, ev.NewVersion, ev.Detail, manualInt, formatTime(createdAt))
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		cp := ev
		cp.ID = id
		cp.Agent = agent
		cp.Host = strings.TrimSpace(ev.Host)
		cp.CreatedAt = createdAt.UTC()
		recorded = &cp
		return nil
	})
	if err != nil {
		return nil, err
	}
	return recorded, nil
}

// ListAgentVersionEvents returns events from agent_version_events ordered by newest first (id DESC).
// When filter.Since is set, only events with created_at strictly after filter.Since are returned.
func (s *Store) ListAgentVersionEvents(ctx context.Context, filter AgentVersionEventsFilter) ([]*AgentVersionEvent, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	if filter.EventType != "" && !filter.EventType.Valid() {
		return nil, fmt.Errorf("%w: invalid agent version event type %q", ErrInvalidInput, filter.EventType)
	}

	var where []string
	var args []any
	if a := strings.TrimSpace(filter.Agent); a != "" {
		where = append(where, "agent = ?")
		args = append(args, a)
	}
	if h := strings.TrimSpace(filter.Host); h != "" {
		where = append(where, "host = ?")
		args = append(args, h)
	}
	if filter.EventType != "" {
		where = append(where, "event_type = ?")
		args = append(args, string(filter.EventType))
	}

	query := `SELECT id, event_type, agent, host, old_version, new_version, detail, manual_update, created_at FROM agent_version_events`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id DESC"
	if filter.Since == nil {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*AgentVersionEvent
	for rows.Next() {
		ev, err := scanAgentVersionEvent(rows)
		if err != nil {
			return nil, err
		}
		if filter.Since != nil && !ev.CreatedAt.After(*filter.Since) {
			continue
		}
		out = append(out, ev)
		if len(out) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func scanAgentVersion(sc rowScanner) (*AgentVersion, error) {
	var (
		v         AgentVersion
		statusRaw string
		checkedAt string
	)
	if err := sc.Scan(&v.Host, &v.Agent, &v.VersionRaw, &v.Version, &statusRaw, &v.ErrorDetail, &checkedAt); err != nil {
		return nil, err
	}
	v.Status = AgentVersionStatus(statusRaw)
	t, err := parseTime(checkedAt)
	if err != nil {
		return nil, fmt.Errorf("parse agent_versions.checked_at %q: %w", checkedAt, err)
	}
	v.CheckedAt = t
	return &v, nil
}

func scanAgentVersionEvent(sc rowScanner) (*AgentVersionEvent, error) {
	var (
		ev        AgentVersionEvent
		typeRaw   string
		manualInt int
		createdAt string
	)
	if err := sc.Scan(&ev.ID, &typeRaw, &ev.Agent, &ev.Host, &ev.OldVersion, &ev.NewVersion, &ev.Detail, &manualInt, &createdAt); err != nil {
		return nil, err
	}
	ev.EventType = AgentVersionEventType(typeRaw)
	ev.ManualUpdate = manualInt != 0
	t, err := parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse agent_version_events.created_at %q: %w", createdAt, err)
	}
	ev.CreatedAt = t
	return &ev, nil
}

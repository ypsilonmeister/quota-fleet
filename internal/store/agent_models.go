package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AgentModelStatus represents the last probe outcome for a (host, agent) model catalog.
type AgentModelStatus string

const (
	AgentModelStatusOK    AgentModelStatus = "ok"
	AgentModelStatusError AgentModelStatus = "error"
)

// Valid reports whether s is a supported AgentModelStatus.
func (s AgentModelStatus) Valid() bool {
	return s == AgentModelStatusOK || s == AgentModelStatusError
}

// AgentModelItem represents a single selectable model in a host × agent catalog.
type AgentModelItem struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Family      string `json:"family,omitempty"`
}

// AgentModelCatalog represents the latest stored model catalog snapshot for a (host, agent) pair.
type AgentModelCatalog struct {
	Host        string
	Agent       string
	Models      []AgentModelItem
	Source      string
	Status      AgentModelStatus
	ErrorDetail string
	CheckedAt   time.Time
	UpdatedAt   *time.Time
}

// UpsertAgentModels inserts or updates the model catalog snapshot for (host, agent) after a successful probe.
func (s *Store) UpsertAgentModels(ctx context.Context, cat AgentModelCatalog) error {
	host := strings.TrimSpace(cat.Host)
	agent := strings.TrimSpace(cat.Agent)
	if host == "" || agent == "" {
		return fmt.Errorf("%w: host and agent must not be empty", ErrInvalidInput)
	}
	checkedAt := cat.CheckedAt
	if checkedAt.IsZero() {
		checkedAt = time.Now().UTC()
	}
	updatedAt := checkedAt
	if cat.UpdatedAt != nil && !cat.UpdatedAt.IsZero() {
		updatedAt = cat.UpdatedAt.UTC()
	}

	models := cat.Models
	if models == nil {
		models = []AgentModelItem{}
	}
	modelsJSON, err := json.Marshal(models)
	if err != nil {
		return fmt.Errorf("marshal agent models: %w", err)
	}

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO agent_models (host, agent, models_json, source, status, error_detail, checked_at, updated_at)
			VALUES (?, ?, ?, ?, 'ok', '', ?, ?)
			ON CONFLICT(host, agent) DO UPDATE SET
				models_json = excluded.models_json,
				source = excluded.source,
				status = 'ok',
				error_detail = '',
				checked_at = excluded.checked_at,
				updated_at = excluded.updated_at
		`, host, agent, string(modelsJSON), strings.TrimSpace(cat.Source), formatTime(checkedAt), formatTime(updatedAt))
		return err
	})
}

// RecordAgentModelsError records a failed model catalog probe for (host, agent).
// If a previous snapshot exists, its models_json, source, and updated_at are preserved untouched
// so callers never lose the last known good catalog on transient probe failures.
func (s *Store) RecordAgentModelsError(ctx context.Context, host, agent, errDetail string, checkedAt time.Time) error {
	host = strings.TrimSpace(host)
	agent = strings.TrimSpace(agent)
	if host == "" || agent == "" {
		return fmt.Errorf("%w: host and agent must not be empty", ErrInvalidInput)
	}
	if checkedAt.IsZero() {
		checkedAt = time.Now().UTC()
	}

	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO agent_models (host, agent, models_json, source, status, error_detail, checked_at, updated_at)
			VALUES (?, ?, '[]', '', 'error', ?, ?, '')
			ON CONFLICT(host, agent) DO UPDATE SET
				status = 'error',
				error_detail = excluded.error_detail,
				checked_at = excluded.checked_at
		`, host, agent, strings.TrimSpace(errDetail), formatTime(checkedAt))
		return err
	})
}

// GetAgentModels fetches the latest stored model catalog for (host, agent).
func (s *Store) GetAgentModels(ctx context.Context, host, agent string) (*AgentModelCatalog, error) {
	host = strings.TrimSpace(host)
	agent = strings.TrimSpace(agent)
	if host == "" || agent == "" {
		return nil, fmt.Errorf("%w: host and agent must not be empty", ErrInvalidInput)
	}

	row := s.readDB.QueryRowContext(ctx, `
		SELECT host, agent, models_json, source, status, error_detail, checked_at, updated_at
		FROM agent_models
		WHERE host = ? AND agent = ?
	`, host, agent)

	cat, err := scanAgentModelCatalog(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return cat, nil
}

// ListAgentModels returns all stored agent_models rows ordered by host, agent.
func (s *Store) ListAgentModels(ctx context.Context) ([]*AgentModelCatalog, error) {
	rows, err := s.readDB.QueryContext(ctx, `
		SELECT host, agent, models_json, source, status, error_detail, checked_at, updated_at
		FROM agent_models
		ORDER BY host ASC, agent ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*AgentModelCatalog
	for rows.Next() {
		cat, err := scanAgentModelCatalog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cat)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func scanAgentModelCatalog(sc rowScanner) (*AgentModelCatalog, error) {
	var (
		cat        AgentModelCatalog
		modelsJSON string
		statusRaw  string
		checkedAt  string
		updatedAt  string
	)
	if err := sc.Scan(&cat.Host, &cat.Agent, &modelsJSON, &cat.Source, &statusRaw, &cat.ErrorDetail, &checkedAt, &updatedAt); err != nil {
		return nil, err
	}
	cat.Status = AgentModelStatus(statusRaw)
	if strings.TrimSpace(modelsJSON) != "" {
		if err := json.Unmarshal([]byte(modelsJSON), &cat.Models); err != nil {
			return nil, fmt.Errorf("unmarshal agent_models.models_json: %w", err)
		}
	}
	if cat.Models == nil {
		cat.Models = []AgentModelItem{}
	}
	ct, err := parseTime(checkedAt)
	if err != nil {
		return nil, fmt.Errorf("parse agent_models.checked_at %q: %w", checkedAt, err)
	}
	cat.CheckedAt = ct
	if strings.TrimSpace(updatedAt) != "" {
		ut, err := parseTime(updatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse agent_models.updated_at %q: %w", updatedAt, err)
		}
		cat.UpdatedAt = &ut
	}
	return &cat, nil
}

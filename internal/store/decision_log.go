package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// DecisionLogEntry is one review verdict judgment (docs/18). The review text is not stored.
type DecisionLogEntry struct {
	ID        int64
	CreatedAt time.Time
	// JobID and ReviewJobID are optional caller-supplied references; they are not checked
	// against the jobs table.
	JobID       *int64
	ReviewJobID *int64
	Verdict     string
	Score       float64
	Model       string
}

// InsertDecisionLog records e and returns its id. e.ID and e.CreatedAt are ignored.
func (s *Store) InsertDecisionLog(ctx context.Context, e DecisionLogEntry) (int64, error) {
	if strings.TrimSpace(e.Verdict) == "" || strings.TrimSpace(e.Model) == "" {
		return 0, fmt.Errorf("%w: decision log needs verdict and model", ErrInvalidInput)
	}
	now := formatTime(time.Now())
	var id int64
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			INSERT INTO decision_log (created_at, job_id, review_job_id, verdict, score, model)
			VALUES (?, ?, ?, ?, ?, ?)
		`, now, e.JobID, e.ReviewJobID, e.Verdict, e.Score, e.Model)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("insert decision log: %w", err)
	}
	return id, nil
}

// ListDecisionLog returns up to limit entries, newest first. limit <= 0 means 100.
func (s *Store) ListDecisionLog(ctx context.Context, limit int) ([]DecisionLogEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.readDB.QueryContext(ctx, `
		SELECT id, created_at, job_id, review_job_id, verdict, score, model
		FROM decision_log ORDER BY id DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list decision log: %w", err)
	}
	defer rows.Close()

	var out []DecisionLogEntry
	for rows.Next() {
		var (
			e                  DecisionLogEntry
			createdAt          string
			jobID, reviewJobID sql.NullInt64
		)
		if err := rows.Scan(&e.ID, &createdAt, &jobID, &reviewJobID, &e.Verdict, &e.Score, &e.Model); err != nil {
			return nil, fmt.Errorf("scan decision log: %w", err)
		}
		if e.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, fmt.Errorf("parse decision log created_at: %w", err)
		}
		if jobID.Valid {
			e.JobID = &jobID.Int64
		}
		if reviewJobID.Valid {
			e.ReviewJobID = &reviewJobID.Int64
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list decision log: %w", err)
	}
	return out, nil
}

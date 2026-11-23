package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/evaluator"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const maxLastErrorLogBytes = 512

// ShepherdOptions configures a Shepherd.
type ShepherdOptions struct {
	Store          *store.Store
	Adapter        adapter.Adapter
	Evaluator      evaluator.Evaluator
	Notifier       Notifier
	LauncherNotify chan<- struct{}
	Interval       time.Duration
	MaxEvalRetries int
	Logger         *log.Logger
}

type evalFailureState struct {
	claimedBy string
	count     int
}

// Shepherd periodically inspects claimed jobs, evaluates exited runs, transitions them
// to done or failed, delivers results via Notifier, and signals Launcher when slots open.
type Shepherd struct {
	store          *store.Store
	adapter        adapter.Adapter
	evaluator      evaluator.Evaluator
	notifier       Notifier
	launcherNotify chan<- struct{}
	interval       time.Duration
	maxEvalRetries int
	logger         *log.Logger

	evalMu       sync.Mutex
	evalFailures map[int64]evalFailureState
}

// NewShepherd creates a new Shepherd.
func NewShepherd(opts ShepherdOptions) *Shepherd {
	interval := opts.Interval
	if interval <= 0 {
		interval = config.DefaultShepherdInterval
	}
	maxEvalRetries := opts.MaxEvalRetries
	if maxEvalRetries <= 0 {
		maxEvalRetries = config.DefaultEvaluatorMaxEvalRetries
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Shepherd{
		store:          opts.Store,
		adapter:        opts.Adapter,
		evaluator:      opts.Evaluator,
		notifier:       opts.Notifier,
		launcherNotify: opts.LauncherNotify,
		interval:       interval,
		maxEvalRetries: maxEvalRetries,
		logger:         logger,
		evalFailures:   make(map[int64]evalFailureState),
	}
}

// Run periodically executes SweepOnce until ctx is cancelled.
func (s *Shepherd) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.SweepOnce(ctx); err != nil && ctx.Err() == nil {
				s.logger.Printf("shepherd sweep error: %v", err)
			}
		}
	}
}

// SweepOnce inspects active claimed jobs. When active Store counts are all zero,
// detailed inspection is skipped immediately (design §1.1).
func (s *Shepherd) SweepOnce(ctx context.Context) error {
	counts, hasUndel, err := checkActiveWork(ctx, s.store)
	if err != nil {
		return err
	}
	if !hasAnyActive(counts, hasUndel) {
		return nil
	}
	if counts[store.StatusClaimed] == 0 {
		return nil
	}

	claimedJobs, err := listAllByStatus(ctx, s.store, store.StatusClaimed)
	if err != nil {
		return fmt.Errorf("list claimed jobs: %w", err)
	}

	for _, job := range claimedJobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.handleClaimedJob(ctx, job)
	}
	return nil
}

func (s *Shepherd) handleClaimedJob(ctx context.Context, job *store.Job) {
	state, err := s.adapter.Status(ctx, job)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Printf("shepherd status error for job #%d: %v", job.ID, err)
		}
		return
	}

	switch state.State {
	case adapter.StateRunning:
		return
	case adapter.StateMissing:
		// Missing claimed jobs are handled by Reconciler.
		return
	case adapter.StateExited:
		s.finishExitedJob(ctx, job, state)
	default:
		s.logger.Printf("shepherd unexpected state %q for job #%d", state.State, job.ID)
	}
}

func (s *Shepherd) finishExitedJob(ctx context.Context, job *store.Job, state adapter.RunState) {
	ev, err := s.adapter.CollectEvidence(ctx, job)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Printf("shepherd collect evidence error for job #%d: %v", job.ID, err)
		}
		return
	}
	if state.ExitCodeUnknown {
		ev.ExitCodeUnknown = true
	} else {
		ev.ExitCode = state.ExitCode
	}

	var terminalJob *store.Job
	if !state.ExitCodeUnknown && state.ExitCode != 0 {
		s.clearEvalFailure(job.ID)
		reason, resultText := formatNonZeroExitFailure(state.ExitCode, ev.FinalReport)
		res, err := s.store.FailWithResult(ctx, job.ID, job.ClaimedBy, reason, resultText)
		if err != nil {
			if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrTokenMismatch) {
				// Lost ownership; stop before delivery per docs/02-job-model.md.
				return
			}
			s.logger.Printf("shepherd fail job #%d (exit_code=%d) error: %v", job.ID, state.ExitCode, err)
			return
		}
		terminalJob = res.Job
		s.logger.Printf("shepherd failed job #%d (exit_code=%d, cancelled_children=%v)", job.ID, state.ExitCode, res.CancelledChildIDs)
	} else {
		evalRes, err := s.evaluator.Evaluate(ctx, job, ev)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			count := s.recordEvalFailure(job.ID, job.ClaimedBy)
			s.logger.Printf("shepherd evaluate error for job #%d (%d/%d): %v", job.ID, count, s.maxEvalRetries, err)
			if count < s.maxEvalRetries {
				return
			}

			reason, resultText := formatEvalErrorFailure(count, s.maxEvalRetries, err)
			res, failErr := s.store.FailWithResult(ctx, job.ID, job.ClaimedBy, reason, resultText)
			if failErr != nil {
				if errors.Is(failErr, store.ErrConflict) || errors.Is(failErr, store.ErrTokenMismatch) {
					s.clearEvalFailure(job.ID)
					return
				}
				s.logger.Printf("shepherd fail job #%d after eval retries error: %v", job.ID, failErr)
				return
			}
			s.clearEvalFailure(job.ID)
			terminalJob = res.Job
			s.logger.Printf("shepherd failed job #%d after %d evaluation errors (cancelled_children=%v)", job.ID, count, res.CancelledChildIDs)
		} else {
			s.clearEvalFailure(job.ID)
			resultText := evalRes.FormatText()

			if evalRes.Passed {
				res, err := s.store.Complete(ctx, job.ID, job.ClaimedBy, resultText)
				if err != nil {
					if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrTokenMismatch) {
						// Lost ownership; stop before delivery per docs/02-job-model.md.
						return
					}
					s.logger.Printf("shepherd complete job #%d error: %v", job.ID, err)
					return
				}
				terminalJob = res.Job
				s.logger.Printf("shepherd completed job #%d (promoted=%v)", job.ID, res.PromotedChildIDs)
			} else {
				reason := evalRes.FailSummary()
				res, err := s.store.FailWithResult(ctx, job.ID, job.ClaimedBy, reason, resultText)
				if err != nil {
					if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrTokenMismatch) {
						return
					}
					s.logger.Printf("shepherd fail job #%d error: %v", job.ID, err)
					return
				}
				terminalJob = res.Job
				s.logger.Printf("shepherd failed job #%d (cancelled_children=%v)", job.ID, res.CancelledChildIDs)
			}
		}
	}

	// Free slot became available; wake Launcher for any already-queued jobs.
	s.notifyLauncher()

	// Deliver terminal job and mark delivered_at (Reconciler recovers if this fails).
	if err := deliverAndMark(ctx, s.store, s.notifier, terminalJob); err != nil && ctx.Err() == nil {
		s.logger.Printf("shepherd delivery error for job #%d: %v", terminalJob.ID, err)
	}
}

func formatNonZeroExitFailure(exitCode int, logTail string) (reason, resultText string) {
	trimmed := strings.TrimSpace(logTail)
	if trimmed == "" {
		return fmt.Sprintf("agent exited with code %d", exitCode),
			fmt.Sprintf("overall: FAIL\nexit_code: %d", exitCode)
	}
	snippet := truncateTailUTF8(trimmed, maxLastErrorLogBytes)
	reason = fmt.Sprintf("agent exited with code %d: %s", exitCode, snippet)
	resultText = fmt.Sprintf("overall: FAIL\nexit_code: %d\nagent_log_tail:\n%s", exitCode, trimmed)
	return reason, resultText
}

func truncateTailUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return "..." + strings.TrimSpace(s[start:])
}

func formatEvalErrorFailure(count, maxRetries int, evalErr error) (reason, resultText string) {
	msg := strings.TrimSpace(evalErr.Error())
	if msg == "" {
		msg = "unknown evaluation error"
	}
	reason = fmt.Sprintf("evaluation failed after %d/%d attempts: %s", count, maxRetries, truncateTailUTF8(msg, maxLastErrorLogBytes))
	resultText = fmt.Sprintf("overall: FAIL\nevaluation_error: %s\neval_attempts: %d/%d", msg, count, maxRetries)
	return reason, resultText
}

func (s *Shepherd) recordEvalFailure(jobID int64, claimedBy string) int {
	s.evalMu.Lock()
	defer s.evalMu.Unlock()
	if s.evalFailures == nil {
		s.evalFailures = make(map[int64]evalFailureState)
	}
	st := s.evalFailures[jobID]
	if st.claimedBy != claimedBy {
		st = evalFailureState{claimedBy: claimedBy, count: 0}
	}
	st.count++
	s.evalFailures[jobID] = st
	return st.count
}

func (s *Shepherd) clearEvalFailure(jobID int64) {
	s.evalMu.Lock()
	defer s.evalMu.Unlock()
	delete(s.evalFailures, jobID)
}

func (s *Shepherd) notifyLauncher() {
	if s.launcherNotify == nil {
		return
	}
	select {
	case s.launcherNotify <- struct{}{}:
	default:
	}
}

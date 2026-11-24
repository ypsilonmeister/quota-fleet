package store

import (
	"context"
	"database/sql"
	"sync"
	"time"
)

// jobEventBuffer is the per-subscriber channel capacity (docs/14). It is large enough that
// one cascade cancel does not overflow a subscriber that is keeping up.
const jobEventBuffer = 256

// JobEvent describes one committed job status transition.
type JobEvent struct {
	JobID  int64
	Title  string
	Status Status
	// PrevStatus is empty for a newly created job.
	PrevStatus Status
	Agent      string
	Host       string
	UpdatedAt  time.Time
}

// statusChange is a transition recorded inside a transaction, published after commit.
type statusChange struct {
	jobID      int64
	prev, next Status
}

// jobEventHub fans committed transitions out to in-process subscribers. The zero value is
// ready to use.
type jobEventHub struct {
	// commitMu orders subscriptions against commits: a subscriber receives a transition
	// only if it subscribed before that transition was committed.
	commitMu sync.Mutex

	mu     sync.Mutex
	subs   map[chan JobEvent]struct{}
	closed bool
}

func (h *jobEventHub) subscribe() (<-chan JobEvent, func()) {
	ch := make(chan JobEvent, jobEventBuffer)
	h.commitMu.Lock()
	defer h.commitMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(ch)
		return ch, func() {}
	}
	if h.subs == nil {
		h.subs = make(map[chan JobEvent]struct{})
	}
	h.subs[ch] = struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.dropLocked(ch)
	}
}

// dropLocked removes and closes ch if it is still subscribed. h.mu must be held.
func (h *jobEventHub) dropLocked(ch chan JobEvent) {
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

// commit runs commitTx and returns the subscribers that existed when it took effect.
// subscribe waits meanwhile; publishing and cancelling do not.
func (h *jobEventHub) commit(commitTx func() error) ([]chan JobEvent, error) {
	h.commitMu.Lock()
	defer h.commitMu.Unlock()
	if err := commitTx(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	recipients := make([]chan JobEvent, 0, len(h.subs))
	for ch := range h.subs {
		recipients = append(recipients, ch)
	}
	return recipients, nil
}

// publish sends ev to the recipients that are still subscribed. It never blocks: a
// subscriber whose buffer is full is dropped (its channel closed).
func (h *jobEventHub) publish(ev JobEvent, recipients []chan JobEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range recipients {
		if _, ok := h.subs[ch]; !ok {
			continue
		}
		select {
		case ch <- ev:
		default:
			h.dropLocked(ch)
		}
	}
}

func (h *jobEventHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.subs {
		h.dropLocked(ch)
	}
}

// SubscribeJobEvents returns a channel of job status transitions committed through this
// Store from now on (writes by other processes are not observed; nothing is replayed).
// The channel is closed when cancel is called, when the subscriber falls too far behind,
// or when CloseJobEvents is called.
func (s *Store) SubscribeJobEvents() (events <-chan JobEvent, cancel func()) {
	return s.events.subscribe()
}

// CloseJobEvents closes every subscription and rejects new ones.
func (s *Store) CloseJobEvents() {
	s.events.close()
}

// withJobTx is withImmediateTx for transactions that change job status. The changes that
// fn appends are published to subscribers only after a successful commit.
func (s *Store) withJobTx(ctx context.Context, fn func(conn *sql.Conn, changes *[]statusChange) error) error {
	var changes []statusChange
	return s.withImmediateTxCommit(ctx, func(conn *sql.Conn) error {
		changes = changes[:0] // fn reruns when the database is busy.
		return fn(conn, &changes)
	}, func(conn *sql.Conn, commitTx func() error) error {
		recipients, err := s.events.commit(commitTx)
		if err != nil {
			return err
		}
		if s.afterJobCommit != nil {
			s.afterJobCommit()
		}
		s.publishJobEvents(context.WithoutCancel(ctx), conn, changes, recipients)
		return nil
	})
}

// publishJobEvents runs while the single write connection is still held, so events are
// published in commit order. The transition is already committed: a failed lookup only
// loses the notification.
func (s *Store) publishJobEvents(ctx context.Context, conn *sql.Conn, changes []statusChange, recipients []chan JobEvent) {
	if len(recipients) == 0 {
		return
	}
	for _, c := range changes {
		ev := JobEvent{JobID: c.jobID, Status: c.next, PrevStatus: c.prev}
		var updatedAt string
		err := conn.QueryRowContext(ctx, `SELECT title, agent, host, updated_at FROM jobs WHERE id = ?`, c.jobID).
			Scan(&ev.Title, &ev.Agent, &ev.Host, &updatedAt)
		if err != nil {
			continue
		}
		if ev.UpdatedAt, err = parseTime(updatedAt); err != nil {
			continue
		}
		s.events.publish(ev, recipients)
	}
}

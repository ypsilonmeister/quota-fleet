package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const (
	// defaultEffortProbeTimeout bounds the `<cli> --help` probe that POST /jobs runs on the job's
	// host when the job has an effort (docs/21 §3).
	defaultEffortProbeTimeout = 15 * time.Second
	// effortProbeTTL is how long a settled probe result (supported or not) is reused for the
	// same host, agent and executable, so a burst of registrations asks the host once.
	effortProbeTTL = 10 * time.Minute
)

// checkEffortLaunch rejects an effort that the job's host could not pass to the agent CLI
// (docs/21 §3). The driver for the job's host and agent is resolved from fleet.yaml (launch /
// resume template and the per-host command override) and must launch the agent's own CLI; then,
// when the server can reach hosts, that CLI on the host is asked for its help, which must list
// the effort option. A CLI with the agent's name that lacks the option (an older release) is
// rejected with 400 like a wrapper. When the host cannot be asked, the answer is 503: the effort
// is neither accepted unchecked nor reported as unsupported.
//
// Without a loaded config or a known host there is nothing to resolve, and the check is skipped.
func (s *Server) checkEffortLaunch(ctx context.Context, p store.CreateJobParams) (int, error) {
	if strings.TrimSpace(p.Effort) == "" || s.cfg == nil {
		return 0, nil
	}
	host := strings.TrimSpace(p.Host)
	hc, ok := s.cfg.Hosts[host]
	if !ok {
		return 0, nil
	}
	invalid := func(err error) (int, error) {
		return http.StatusBadRequest, fmt.Errorf("%w: host %q: %s", store.ErrInvalidInput, host, strings.TrimPrefix(err.Error(), "adapter: "))
	}
	drv, err := adapter.ResolveDriver(s.cfg, hc, p.Agent)
	if err != nil {
		return invalid(err)
	}
	job := &store.Job{Agent: p.Agent, Effort: p.Effort, SessionID: p.SessionID}
	if err := drv.CheckEffort(job); err != nil {
		return invalid(err)
	}
	if s.effortProbe == nil {
		return 0, nil
	}
	isWindows := strings.EqualFold(strings.TrimSpace(hc.OS), "windows")
	probe, err := drv.BuildEffortProbe(job, isWindows)
	if err != nil {
		return invalid(err)
	}
	if probe == nil {
		return 0, nil
	}
	if err := s.effortProbe.check(ctx, host, strings.TrimSpace(p.Agent), probe); err != nil {
		if errors.Is(err, adapter.ErrEffortUnsupported) {
			return invalid(err)
		}
		return http.StatusServiceUnavailable, fmt.Errorf("cannot confirm effort support on host %q: %s", host, strings.TrimPrefix(err.Error(), "adapter: "))
	}
	return 0, nil
}

// effortProber runs effort probes on hosts and remembers settled results for effortProbeTTL.
// Failures to reach the host are not remembered, so the next registration asks again.
type effortProber struct {
	hosts   adapter.HostFileAccessProvider
	timeout time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache map[effortProbeKey]effortProbeResult
}

type effortProbeKey struct {
	host, agent, exe string
}

type effortProbeResult struct {
	err error // nil when supported, else wraps adapter.ErrEffortUnsupported
	at  time.Time
}

func newEffortProber(hosts adapter.HostFileAccessProvider, timeout time.Duration, now func() time.Time) *effortProber {
	if hosts == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = defaultEffortProbeTimeout
	}
	if now == nil {
		now = time.Now
	}
	return &effortProber{hosts: hosts, timeout: timeout, now: now, cache: make(map[effortProbeKey]effortProbeResult)}
}

func (e *effortProber) check(ctx context.Context, host, agent string, probe *adapter.EffortProbe) error {
	key := effortProbeKey{host: host, agent: agent, exe: probe.Executable}
	e.mu.Lock()
	if r, ok := e.cache[key]; ok && e.now().Sub(r.at) < effortProbeTTL {
		e.mu.Unlock()
		return r.err
	}
	e.mu.Unlock()

	fa, err := e.hosts.FileAccessForHost(host)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	res, runErr := fa.RunCommand(probeCtx, probe.Command)
	err = probe.Classify(res, runErr)
	if err == nil || errors.Is(err, adapter.ErrEffortUnsupported) {
		e.mu.Lock()
		e.cache[key] = effortProbeResult{err: err, at: e.now()}
		e.mu.Unlock()
	}
	return err
}

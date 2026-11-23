package adapter

import (
	"context"
	"strings"
	"sync"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

// FakeCall records a single Transport.Run invocation captured by FakeTransport.
type FakeCall struct {
	Host    config.HostConfig
	Command string
}

// FakeRule matches an incoming command and produces a synthetic response.
type FakeRule struct {
	Match  func(host config.HostConfig, cmd string) bool
	Handle func(ctx context.Context, host config.HostConfig, cmd string) (CommandResult, error)
}

// FakeTransport is a thread-safe Transport implementation for unit tests that records
// commands and returns programmable responses without touching real SSH or local shells.
type FakeTransport struct {
	mu         sync.Mutex
	calls      []FakeCall
	rules      []FakeRule
	handler    func(ctx context.Context, host config.HostConfig, cmd string) (CommandResult, error)
	defaultRes CommandResult
	defaultErr error
}

// NewFakeTransport constructs an empty FakeTransport whose default response is exit code 0.
func NewFakeTransport() *FakeTransport {
	return &FakeTransport{}
}

// SetHandler sets a custom handler invoked when no explicit rule matches.
func (f *FakeTransport) SetHandler(fn func(ctx context.Context, host config.HostConfig, cmd string) (CommandResult, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = fn
}

// SetDefault configures the fallback response when neither rules nor handler match.
func (f *FakeTransport) SetDefault(res CommandResult, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultRes = res
	f.defaultErr = err
}

// OnMatch registers a rule evaluated in registration order.
func (f *FakeTransport) OnMatch(
	match func(host config.HostConfig, cmd string) bool,
	handle func(ctx context.Context, host config.HostConfig, cmd string) (CommandResult, error),
) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, FakeRule{Match: match, Handle: handle})
}

// OnContains registers a rule returning (res, err) whenever cmd contains substr.
func (f *FakeTransport) OnContains(substr string, res CommandResult, err error) {
	f.OnMatch(
		func(_ config.HostConfig, cmd string) bool { return strings.Contains(cmd, substr) },
		func(_ context.Context, _ config.HostConfig, _ string) (CommandResult, error) { return res, err },
	)
}

// OnContainsFunc registers a dynamic rule invoked whenever cmd contains substr.
func (f *FakeTransport) OnContainsFunc(
	substr string,
	fn func(ctx context.Context, host config.HostConfig, cmd string) (CommandResult, error),
) {
	f.OnMatch(
		func(_ config.HostConfig, cmd string) bool { return strings.Contains(cmd, substr) },
		fn,
	)
}

// Run records the invocation and dispatches to the first matching rule, custom handler, or default.
func (f *FakeTransport) Run(ctx context.Context, host config.HostConfig, cmd string) (CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return CommandResult{}, err
	}

	f.mu.Lock()
	f.calls = append(f.calls, FakeCall{Host: host, Command: cmd})
	rules := append([]FakeRule(nil), f.rules...)
	handler := f.handler
	defRes := f.defaultRes
	defErr := f.defaultErr
	f.mu.Unlock()

	for _, rule := range rules {
		if rule.Match != nil && rule.Match(host, cmd) {
			return rule.Handle(ctx, host, cmd)
		}
	}
	if handler != nil {
		return handler(ctx, host, cmd)
	}
	return defRes, defErr
}

// Calls returns a snapshot of all recorded FakeCall entries in invocation order.
func (f *FakeTransport) Calls() []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// Reset clears recorded calls while preserving configured rules.
func (f *FakeTransport) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

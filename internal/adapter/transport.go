package adapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

const (
	// DefaultTransportTimeout is the default per-command execution timeout for transports.
	DefaultTransportTimeout = 30 * time.Second
	// DefaultSSHConnectTimeout is the default OpenSSH ConnectTimeout option.
	DefaultSSHConnectTimeout = 10 * time.Second
)

// CommandResult captures stdout, stderr, and exit code from a Transport.Run call.
type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Transport executes a shell command on a worker host (remote via SSH, locally, or faked in tests).
// Normal non-zero command exit codes are returned via CommandResult.ExitCode with a nil error.
// A non-nil error indicates a transport, timeout, cancellation, or invocation failure.
type Transport interface {
	Run(ctx context.Context, host config.HostConfig, cmd string) (CommandResult, error)
}

// SSHTransportOptions configures SSHTransport.
type SSHTransportOptions struct {
	SSHBinary      string
	Timeout        time.Duration
	ConnectTimeout time.Duration
}

// SSHTransport invokes the system ssh binary in non-interactive BatchMode with timeouts.
type SSHTransport struct {
	sshBinary      string
	timeout        time.Duration
	connectTimeout time.Duration
}

// NewSSHTransport constructs an SSHTransport with sensible defaults.
func NewSSHTransport(opts SSHTransportOptions) *SSHTransport {
	bin := strings.TrimSpace(opts.SSHBinary)
	if bin == "" {
		bin = "ssh"
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTransportTimeout
	}
	connectTimeout := opts.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = DefaultSSHConnectTimeout
	}
	return &SSHTransport{
		sshBinary:      bin,
		timeout:        timeout,
		connectTimeout: connectTimeout,
	}
}

// BuildArgs constructs the non-interactive OpenSSH command-line arguments for host and cmd.
func (t *SSHTransport) BuildArgs(host config.HostConfig, cmd string) ([]string, error) {
	target := strings.TrimSpace(host.SSH)
	if target == "" {
		return nil, errors.New("ssh transport: host ssh target must not be empty")
	}

	connectSec := int(t.connectTimeout / time.Second)
	if connectSec <= 0 {
		connectSec = 1
	}

	args := []string{
		"-o", "BatchMode=yes",
		"-o", fmt.Sprintf("ConnectTimeout=%d", connectSec),
	}

	if keyFile := strings.TrimSpace(host.KeyFile); keyFile != "" {
		args = append(args, "-i", expandLocalTilde(keyFile), "-o", "IdentitiesOnly=yes")
	}

	if host.ProxyCommand != nil {
		if proxy := strings.TrimSpace(*host.ProxyCommand); proxy != "" {
			args = append(args, "-o", "ProxyCommand="+proxy)
		}
	}

	if knownHosts := strings.TrimSpace(host.KnownHostsFile); knownHosts != "" {
		args = append(args, "-o", "UserKnownHostsFile="+expandLocalTilde(knownHosts))
	}

	if strict := strings.TrimSpace(host.StrictHostKeyChecking); strict != "" {
		args = append(args, "-o", "StrictHostKeyChecking="+strict)
	}

	args = append(args, "--", target, cmd)
	return args, nil
}

// Run executes cmd on host via the system ssh binary.
func (t *SSHTransport) Run(ctx context.Context, host config.HostConfig, cmd string) (CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return CommandResult{}, err
	}
	args, err := t.BuildArgs(host, cmd)
	if err != nil {
		return CommandResult{}, err
	}

	runCtx, cancel := withTransportTimeout(ctx, t.timeout)
	defer cancel()

	c := exec.CommandContext(runCtx, t.sshBinary, args...)
	c.Stdin = bytes.NewReader(nil)
	c.WaitDelay = 500 * time.Millisecond
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	err = c.Run()
	res := CommandResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if err == nil {
		return res, nil
	}

	if runCtx.Err() != nil {
		return res, fmt.Errorf("ssh transport timeout/cancelled: %w", runCtx.Err())
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		// OpenSSH reserves exit code 255 for SSH connection/transport errors.
		if res.ExitCode == 255 {
			detail := strings.TrimSpace(res.Stderr)
			if detail == "" {
				detail = err.Error()
			}
			return res, fmt.Errorf("ssh transport error (exit 255): %s", detail)
		}
		return res, nil
	}

	return res, fmt.Errorf("ssh transport exec %q: %w", t.sshBinary, err)
}

// LocalTransportOptions configures LocalTransport.
type LocalTransportOptions struct {
	Shell   string
	Timeout time.Duration
}

// LocalTransport executes shell commands locally (for worker hosts running on the serve host).
type LocalTransport struct {
	shell   string
	timeout time.Duration
}

// NewLocalTransport constructs a LocalTransport with sensible defaults.
func NewLocalTransport(opts LocalTransportOptions) *LocalTransport {
	shell := strings.TrimSpace(opts.Shell)
	if shell == "" {
		shell = "sh"
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTransportTimeout
	}
	return &LocalTransport{
		shell:   shell,
		timeout: timeout,
	}
}

// Run executes cmd locally via `<shell> -c <cmd>`.
func (t *LocalTransport) Run(ctx context.Context, _ config.HostConfig, cmd string) (CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return CommandResult{}, err
	}

	runCtx, cancel := withTransportTimeout(ctx, t.timeout)
	defer cancel()

	c := exec.CommandContext(runCtx, t.shell, "-c", cmd)
	c.Stdin = bytes.NewReader(nil)
	c.WaitDelay = 500 * time.Millisecond
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	err := c.Run()
	res := CommandResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if err == nil {
		return res, nil
	}

	if runCtx.Err() != nil {
		return res, fmt.Errorf("local transport timeout/cancelled: %w", runCtx.Err())
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}

	return res, fmt.Errorf("local transport exec %q: %w", t.shell, err)
}

// RoutingTransport dispatches Run calls to SSHTransport or LocalTransport based on host.Transport.
type RoutingTransport struct {
	ssh   Transport
	local Transport
}

// NewRoutingTransport creates a RoutingTransport. Nil sub-transports fall back to defaults.
func NewRoutingTransport(sshTransport, localTransport Transport) *RoutingTransport {
	if sshTransport == nil {
		sshTransport = NewSSHTransport(SSHTransportOptions{})
	}
	if localTransport == nil {
		localTransport = NewLocalTransport(LocalTransportOptions{})
	}
	return &RoutingTransport{
		ssh:   sshTransport,
		local: localTransport,
	}
}

// Run routes the command execution according to host.Transport ("local" or "ssh").
func (r *RoutingTransport) Run(ctx context.Context, host config.HostConfig, cmd string) (CommandResult, error) {
	mode := strings.ToLower(strings.TrimSpace(host.Transport))
	switch mode {
	case "local":
		return r.local.Run(ctx, host, cmd)
	case "ssh":
		return r.ssh.Run(ctx, host, cmd)
	case "":
		if strings.TrimSpace(host.SSH) != "" {
			return r.ssh.Run(ctx, host, cmd)
		}
		return r.local.Run(ctx, host, cmd)
	default:
		return CommandResult{}, fmt.Errorf("transport: unsupported host transport %q", host.Transport)
	}
}

func withTransportTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func expandLocalTilde(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// parseExitCode parses either a bare integer ("0", "1") or "EXIT:<code>" into an int.
func parseExitCode(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "EXIT:")
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty exit code")
	}
	return strconv.Atoi(s)
}

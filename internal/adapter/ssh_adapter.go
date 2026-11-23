package adapter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// SSHAdapterOptions configures SSHAdapter.
type SSHAdapterOptions struct {
	// Transport executes commands on target hosts. Defaults to RoutingTransport(SSH, Local).
	Transport Transport
	// ArtifactFiles overrides DefaultArtifactFiles checked during CollectEvidence.
	ArtifactFiles []string
	// LogTailLines sets how many lines of agent.log to collect into Evidence.FinalReport.
	LogTailLines int
	// LogMaxBytes caps Evidence.FinalReport byte length.
	LogMaxBytes int
	// WindowsLauncher optionally overrides the default unimplemented Windows stub.
	WindowsLauncher OSLauncher
}

// SSHAdapter is the real Host/Agent Adapter backed by a Transport (SSH or Local)
// and OS-specific launchers (Linux implemented; Windows stubbed for future WMI support).
type SSHAdapter struct {
	cfg       *config.Config
	transport Transport
	linux     OSLauncher
	windows   OSLauncher
}

// NewSSHAdapter constructs an SSHAdapter from cfg and opts.
func NewSSHAdapter(cfg *config.Config, opts SSHAdapterOptions) *SSHAdapter {
	tr := opts.Transport
	if tr == nil {
		tr = NewRoutingTransport(
			NewSSHTransport(SSHTransportOptions{}),
			NewLocalTransport(LocalTransportOptions{}),
		)
	}
	win := opts.WindowsLauncher
	if win == nil {
		win = newWindowsLauncher()
	}
	return &SSHAdapter{
		cfg:       cfg,
		transport: tr,
		linux:     newLinuxLauncher(tr, opts.ArtifactFiles, opts.LogTailLines, opts.LogMaxBytes),
		windows:   win,
	}
}

// PrepareWorkdir ensures the job's workdir exists (or creates/reuses a git worktree when WorktreeSpec is set).
func (a *SSHAdapter) PrepareWorkdir(ctx context.Context, job *store.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	host, launcher, workdir, err := a.resolveTarget(job)
	if err != nil {
		return err
	}
	if err := checkDeniedPaths(host, job, workdir); err != nil {
		return err
	}
	return launcher.PrepareWorkdir(ctx, host, job, workdir)
}

// Launch writes prompt.txt and run.sh, then detaches the configured agent process on the worker host.
func (a *SSHAdapter) Launch(ctx context.Context, job *store.Job) (LaunchInfo, error) {
	if err := ctx.Err(); err != nil {
		return LaunchInfo{}, err
	}
	host, launcher, workdir, err := a.resolveTarget(job)
	if err != nil {
		return LaunchInfo{}, err
	}
	if err := checkDeniedPaths(host, job, workdir); err != nil {
		return LaunchInfo{}, err
	}
	driver, err := ResolveDriver(a.cfg, host, job.Agent)
	if err != nil {
		return LaunchInfo{}, err
	}
	allowSandbox := ShouldAttachSandboxFlag(host, job, workdir)
	return launcher.Launch(ctx, host, job, workdir, LaunchSpec{
		Driver:       driver,
		AllowSandbox: allowSandbox,
	})
}

// Status inspects agent.pid liveness and agent.exit to classify the run as Running, Exited, or Missing.
func (a *SSHAdapter) Status(ctx context.Context, job *store.Job) (RunState, error) {
	if err := ctx.Err(); err != nil {
		return RunState{}, err
	}
	host, launcher, workdir, err := a.resolveTarget(job)
	if err != nil {
		return RunState{}, err
	}
	return launcher.Status(ctx, host, job, workdir)
}

// CollectEvidence collects git diff status, artifact presence, agent.log tail, and exit code.
func (a *SSHAdapter) CollectEvidence(ctx context.Context, job *store.Job) (Evidence, error) {
	if err := ctx.Err(); err != nil {
		return Evidence{}, err
	}
	host, launcher, workdir, err := a.resolveTarget(job)
	if err != nil {
		return Evidence{}, err
	}
	return launcher.CollectEvidence(ctx, host, job, workdir)
}

// Preflight runs the configured agent preflight command on hostName via the adapter's Transport.
func (a *SSHAdapter) Preflight(ctx context.Context, hostName, agentName string) (CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return CommandResult{}, err
	}
	host, err := a.lookupHost(hostName)
	if err != nil {
		return CommandResult{}, err
	}
	if strings.EqualFold(strings.TrimSpace(host.OS), "windows") {
		return CommandResult{}, fmt.Errorf("%w: windows preflight is not yet implemented", ErrUnsupportedOS)
	}
	driver, err := ResolveDriver(a.cfg, host, agentName)
	if err != nil {
		return CommandResult{}, err
	}
	cmd, err := driver.BuildPreflightCommand()
	if err != nil {
		return CommandResult{}, err
	}
	return a.transport.Run(ctx, host, cmd)
}

func (a *SSHAdapter) resolveTarget(job *store.Job) (config.HostConfig, OSLauncher, string, error) {
	if job == nil {
		return config.HostConfig{}, nil, "", ErrNilJob
	}
	host, err := a.lookupHost(job.Host)
	if err != nil {
		return config.HostConfig{}, nil, "", err
	}
	launcher, err := a.launcherForOS(host.OS)
	if err != nil {
		return config.HostConfig{}, nil, "", err
	}
	workdir, err := resolveWorkdir(host, job)
	if err != nil {
		return config.HostConfig{}, nil, "", err
	}
	return host, launcher, workdir, nil
}

func (a *SSHAdapter) lookupHost(name string) (config.HostConfig, error) {
	if a.cfg == nil {
		return config.HostConfig{}, errors.New("adapter: config must not be nil")
	}
	hostName := strings.TrimSpace(name)
	if hostName == "" {
		return config.HostConfig{}, errors.New("adapter: job host must not be empty")
	}
	host, ok := a.cfg.Hosts[hostName]
	if !ok {
		return config.HostConfig{}, fmt.Errorf("adapter: unknown host %q", hostName)
	}
	return host, nil
}

func (a *SSHAdapter) launcherForOS(osName string) (OSLauncher, error) {
	switch strings.ToLower(strings.TrimSpace(osName)) {
	case "", "linux":
		return a.linux, nil
	case "windows":
		return a.windows, nil
	default:
		return nil, fmt.Errorf("%w: unsupported host os %q", ErrUnsupportedOS, osName)
	}
}

func resolveWorkdir(host config.HostConfig, job *store.Job) (string, error) {
	if wd := strings.TrimSpace(job.Workdir); wd != "" {
		return wd, nil
	}
	if root := strings.TrimSpace(host.WorkdirRoot); root != "" && job.ID > 0 {
		return joinRemotePath(root, fmt.Sprintf("job-%d", job.ID)), nil
	}
	return "", errors.New("adapter: job workdir must not be empty")
}

func checkDeniedPaths(host config.HostConfig, job *store.Job, workdir string) error {
	if isPathDenied(workdir, host.DenyPaths) {
		return fmt.Errorf("%w: workdir %q", ErrDeniedPath, workdir)
	}
	if job.WorktreeSpec != nil && strings.TrimSpace(job.WorktreeSpec.RepoPath) != "" {
		if isPathDenied(job.WorktreeSpec.RepoPath, host.DenyPaths) {
			return fmt.Errorf("%w: repo_path %q", ErrDeniedPath, job.WorktreeSpec.RepoPath)
		}
	}
	return nil
}

package adapter

import (
	"context"
	"fmt"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// windowsLauncher is the OSLauncher dispatch stub for Windows worker hosts.
// WMI Win32_Process.Create support will replace these stubs in a subsequent phase.
type windowsLauncher struct{}

func newWindowsLauncher() *windowsLauncher {
	return &windowsLauncher{}
}

func (w *windowsLauncher) PrepareWorkdir(_ context.Context, _ config.HostConfig, _ *store.Job, _ string) error {
	return fmt.Errorf("%w: windows WMI launcher is not yet implemented", ErrUnsupportedOS)
}

func (w *windowsLauncher) Launch(_ context.Context, _ config.HostConfig, _ *store.Job, _ string, _ LaunchSpec) (LaunchInfo, error) {
	return LaunchInfo{}, fmt.Errorf("%w: windows WMI launcher is not yet implemented", ErrUnsupportedOS)
}

func (w *windowsLauncher) Status(_ context.Context, _ config.HostConfig, _ *store.Job, _ string) (RunState, error) {
	return RunState{}, fmt.Errorf("%w: windows WMI launcher is not yet implemented", ErrUnsupportedOS)
}

func (w *windowsLauncher) CollectEvidence(_ context.Context, _ config.HostConfig, _ *store.Job, _ string) (Evidence, error) {
	return Evidence{}, fmt.Errorf("%w: windows WMI launcher is not yet implemented", ErrUnsupportedOS)
}

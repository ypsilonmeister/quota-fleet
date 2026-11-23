package evaluator

import "github.com/ypsilonmeister/quota-fleet/internal/adapter"

// FileStat holds basic filesystem metadata for a path on a job's target host.
type FileStat = adapter.FileStat

// ExecResult captures the outcome of running a shell command in a job's workdir.
type ExecResult = adapter.ExecResult

// FileAccess abstracts host-aware file inspection, file reading, and command execution
// scoped to a job's workdir on its target host.
type FileAccess = adapter.FileAccess

// LocalFileAccess implements FileAccess directly against the serve host's local filesystem and shell.
type LocalFileAccess = adapter.LocalFileAccess

// NewLocalFileAccess creates a FileAccess backed by the serve host's local filesystem and shell.
func NewLocalFileAccess(workdir string) *LocalFileAccess {
	return adapter.NewLocalFileAccess(workdir)
}

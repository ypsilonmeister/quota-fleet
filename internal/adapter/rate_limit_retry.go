package adapter

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ypsilonmeister/quota-fleet/internal/chain"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// chainRunIDPattern bounds the run id that is placed into a path to delete (run ids are
// UUIDs, chain.NewRunID); anything else is refused instead of being interpolated.
var chainRunIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// retryLogName is where the log of the run that ended in requeue n is kept (Issue #56 §5).
func retryLogName(base string, n int) string {
	return fmt.Sprintf("%s.retry-%d.log", strings.TrimSuffix(base, ".log"), n)
}

// retryVerdictPath returns the workdir-relative verdict file of the job's chain step that
// must be removed before a rate-limit re-run, or "" when there is none. The path is the
// job's own seq, so only a review step can have a file there; other seqs are not touched.
// The step keeps its verdict nonce across a requeue, so a verdict left by the interrupted
// run would otherwise be accepted although the re-run did not write it.
func retryVerdictPath(job *store.Job) (string, error) {
	if job.RateLimitRetries <= 0 || job.ChainRunID == "" || job.ChainSeq <= 0 {
		return "", nil
	}
	if !chainRunIDPattern.MatchString(job.ChainRunID) {
		return "", fmt.Errorf("adapter: refusing verdict cleanup for unexpected chain run id %q", job.ChainRunID)
	}
	return chain.VerdictPath(job.ChainRunID, job.ChainSeq), nil
}

// linuxRetryPrepCommand returns the shell commands run before staging a rate-limit re-run:
// the previous agent.log is moved to agent.retry-<n>.log and the chain review verdict of
// this step is removed. It is "" for a first run.
func linuxRetryPrepCommand(workdir, runDir string, job *store.Job) (string, error) {
	if job.RateLimitRetries <= 0 {
		return "", nil
	}
	logPath := joinRemotePath(runDir, logFileName)
	keepPath := joinRemotePath(runDir, retryLogName(logFileName, job.RateLimitRetries))
	cmds := []string{fmt.Sprintf("if [ -f %s ]; then mv -f %s %s; fi", shellPath(logPath), shellPath(logPath), shellPath(keepPath))}
	verdict, err := retryVerdictPath(job)
	if err != nil {
		return "", err
	}
	if verdict != "" {
		cmds = append(cmds, "rm -f "+shellPath(joinRemotePath(workdir, verdict)))
	}
	return strings.Join(cmds, " && "), nil
}

// windowsRetryPrepStatements is the PowerShell counterpart of linuxRetryPrepCommand
// (agent.log and agent.err.log are both kept).
func windowsRetryPrepStatements(winWorkdir, winRunDir string, job *store.Job) ([]string, error) {
	if job.RateLimitRetries <= 0 {
		return nil, nil
	}
	var stmts []string
	for _, name := range []string{logFileName, errLogFileName} {
		from := joinWindowsPath(winRunDir, name)
		to := joinWindowsPath(winRunDir, retryLogName(name, job.RateLimitRetries))
		stmts = append(stmts, fmt.Sprintf("if (Test-Path -LiteralPath %s -PathType Leaf) { Move-Item -Force -LiteralPath %s -Destination %s }", psExpr(from), psExpr(from), psExpr(to)))
	}
	verdict, err := retryVerdictPath(job)
	if err != nil {
		return nil, err
	}
	if verdict != "" {
		stmts = append(stmts, "Remove-Item -Force -ErrorAction SilentlyContinue -LiteralPath "+psExpr(joinWindowsPath(winWorkdir, verdict)))
	}
	return stmts, nil
}

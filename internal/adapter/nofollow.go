package adapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LinkStat is the result of StatNoFollow.
type LinkStat struct {
	Exists bool
	// Symlink is true when the path or any directory between the workdir and it is a
	// symbolic link (or, on Windows, a reparse point).
	Symlink   bool
	IsDir     bool
	IsRegular bool
	Size      int64
}

// NoFollowStatter is implemented by FileAccess values that can inspect a workdir-relative
// path without following symbolic links (docs/22 §7.1: the review verdict must be a regular
// file inside the workdir).
type NoFollowStatter interface {
	StatNoFollow(ctx context.Context, relPath string) (LinkStat, error)
}

// ErrNotRelative is returned for absolute paths or paths that leave the workdir.
var ErrNotRelative = errors.New("path must be relative to the workdir and stay inside it")

// relComponents splits a slash-separated relative path, rejecting absolute paths, empty
// components, "." and "..".
func relComponents(rel string) ([]string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return nil, ErrEmptyPath
	}
	if strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) || strings.Contains(rel, `\`) || strings.Contains(rel, ":") {
		return nil, fmt.Errorf("%w: %q", ErrNotRelative, rel)
	}
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return nil, fmt.Errorf("%w: %q", ErrNotRelative, rel)
		}
	}
	return parts, nil
}

// StatNoFollow implements NoFollowStatter on the local filesystem.
func (l *LocalFileAccess) StatNoFollow(ctx context.Context, relPath string) (LinkStat, error) {
	if err := ctx.Err(); err != nil {
		return LinkStat{}, err
	}
	parts, err := relComponents(relPath)
	if err != nil {
		return LinkStat{}, err
	}
	wd := strings.TrimSpace(l.workdir)
	if wd == "" {
		return LinkStat{}, ErrEmptyWorkdir
	}
	full := filepath.Join(append([]string{wd}, parts...)...)
	if len(l.denyPaths) > 0 && isPathDenied(full, l.denyPaths) {
		return LinkStat{}, fmt.Errorf("%w: %s", ErrDeniedPath, relPath)
	}
	cur := wd
	var info os.FileInfo
	for _, p := range parts {
		cur = filepath.Join(cur, p)
		info, err = os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return LinkStat{}, nil
		}
		if err != nil {
			return LinkStat{}, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return LinkStat{Exists: true, Symlink: true}, nil
		}
	}
	return LinkStat{Exists: true, IsDir: info.IsDir(), IsRegular: info.Mode().IsRegular(), Size: info.Size()}, nil
}

// StatNoFollow implements NoFollowStatter on a Linux host over the Transport.
func (f *linuxFileAccess) StatNoFollow(ctx context.Context, relPath string) (LinkStat, error) {
	if err := ctx.Err(); err != nil {
		return LinkStat{}, err
	}
	parts, err := relComponents(relPath)
	if err != nil {
		return LinkStat{}, err
	}
	wd := strings.TrimSpace(f.workdir)
	if wd == "" {
		return LinkStat{}, ErrEmptyWorkdir
	}
	if len(f.host.DenyPaths) > 0 && isPathDenied(joinRemotePath(wd, strings.Join(parts, "/")), f.host.DenyPaths) {
		return LinkStat{}, fmt.Errorf("%w: %s", ErrDeniedPath, relPath)
	}
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = shellQuote(p)
	}
	cmd := fmt.Sprintf(
		`cd %s || exit 3; p=''; for c in %s; do p="${p:+$p/}$c"; if [ -L "$p" ]; then printf 'LINK\n'; exit 0; fi; if [ ! -e "$p" ]; then printf 'MISSING\n'; exit 0; fi; done; `+
			`if [ -d "$p" ]; then printf 'DIR\n'; elif [ -f "$p" ]; then printf 'FILE:%%s\n' "$(wc -c < "$p" | tr -d ' \t\r\n')"; else printf 'OTHER\n'; fi`,
		shellPath(wd), strings.Join(quoted, " "),
	)
	res, err := f.transport.Run(ctx, f.host, cmd)
	if err != nil {
		return LinkStat{}, err
	}
	if res.ExitCode != 0 {
		return LinkStat{}, fmt.Errorf("remote no-follow stat failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return parseNoFollowOutput(res.Stdout)
}

// StatNoFollow implements NoFollowStatter on a Windows host (reparse points count as links).
func (f *windowsFileAccess) StatNoFollow(ctx context.Context, relPath string) (LinkStat, error) {
	if err := ctx.Err(); err != nil {
		return LinkStat{}, err
	}
	parts, err := relComponents(relPath)
	if err != nil {
		return LinkStat{}, err
	}
	wd := normalizeWindowsPath(f.workdir)
	if wd == "" {
		return LinkStat{}, ErrEmptyWorkdir
	}
	if len(f.host.DenyPaths) > 0 && isPathDenied(joinWindowsPath(wd, strings.Join(parts, `\`)), f.host.DenyPaths) {
		return LinkStat{}, fmt.Errorf("%w: %s", ErrDeniedPath, relPath)
	}
	items := make([]string, len(parts))
	for i, p := range parts {
		items[i] = psSafeExpr(p)
	}
	ps := fmt.Sprintf(
		"[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; $p = %s; "+
			"foreach ($c in @(%s)) { $p = Join-Path $p $c; "+
			"if (-not (Test-Path -LiteralPath $p)) { Write-Output 'MISSING'; exit 0 }; "+
			"$i = Get-Item -LiteralPath $p -Force; "+
			"if ($i.Attributes -band [System.IO.FileAttributes]::ReparsePoint) { Write-Output 'LINK'; exit 0 } }; "+
			"if ($i.PSIsContainer) { Write-Output 'DIR' } else { Write-Output ('FILE:' + $i.Length) }",
		psSafeExpr(wd), strings.Join(items, ", "),
	)
	res, err := f.transport.Run(ctx, f.host, wrapPowerShellCommand(ps))
	if err != nil {
		return LinkStat{}, err
	}
	if res.ExitCode != 0 {
		return LinkStat{}, fmt.Errorf("remote windows no-follow stat failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return parseNoFollowOutput(res.Stdout)
}

func parseNoFollowOutput(stdout string) (LinkStat, error) {
	line := strings.TrimSpace(stdout)
	switch {
	case line == "MISSING":
		return LinkStat{}, nil
	case line == "LINK":
		return LinkStat{Exists: true, Symlink: true}, nil
	case line == "DIR":
		return LinkStat{Exists: true, IsDir: true}, nil
	case line == "OTHER":
		return LinkStat{Exists: true}, nil
	case strings.HasPrefix(line, "FILE:"):
		n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "FILE:")), 10, 64)
		if err != nil {
			return LinkStat{}, fmt.Errorf("parse remote file size %q: %w", line, err)
		}
		return LinkStat{Exists: true, IsRegular: true, Size: n}, nil
	}
	return LinkStat{}, fmt.Errorf("unexpected remote no-follow stat output: %q", line)
}

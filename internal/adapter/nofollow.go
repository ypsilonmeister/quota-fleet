package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LinkStat is the kind of a workdir-relative path as seen without following links.
type LinkStat struct {
	Exists bool
	// Symlink is true when the path or any directory between the workdir and it is a
	// symbolic link (or, on Windows, a reparse point).
	Symlink   bool
	IsDir     bool
	IsRegular bool
	Size      int64
}

// NoFollowRead is the result of ReadFileNoFollow.
type NoFollowRead struct {
	LinkStat
	// Changed is true when the path changed between the link check and the read (it
	// became a link, disappeared, or no longer names the file that was opened).
	Changed bool
	// TooLarge is true when the file has more than maxBytes bytes; Data is then empty.
	TooLarge bool
	// Data is the content of the regular file that was checked (at most maxBytes).
	Data []byte
}

// NoFollowReader is implemented by FileAccess values that can read a workdir-relative
// regular file without following symbolic links (docs/22 §7.1: the review verdict must
// be a regular file inside the workdir, at most a few KiB).
//
// The check and the read are one operation: the file is opened after the link check,
// the path is checked again while the file is open, and the open file must be the file
// the path names (same device and inode; on Windows the file is opened without
// delete/write sharing so it cannot be replaced while open). At most maxBytes+1 bytes are read.
type NoFollowReader interface {
	ReadFileNoFollow(ctx context.Context, relPath string, maxBytes int64) (NoFollowRead, error)
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

// noFollowHooks are set only by tests to change the filesystem at the race points.
var noFollowHooks struct {
	beforeOpen func()
	afterOpen  func()
}

// lstatChain walks the components below wd with Lstat. It returns the final FileInfo,
// or a LinkStat describing why the walk stopped (missing or link).
func lstatChain(wd string, parts []string) (os.FileInfo, *LinkStat, error) {
	cur := wd
	var info os.FileInfo
	var err error
	for _, p := range parts {
		cur = filepath.Join(cur, p)
		info, err = os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil, &LinkStat{}, nil
		}
		if err != nil {
			return nil, nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, &LinkStat{Exists: true, Symlink: true}, nil
		}
	}
	return info, nil, nil
}

// ReadFileNoFollow implements NoFollowReader on the local filesystem.
func (l *LocalFileAccess) ReadFileNoFollow(ctx context.Context, relPath string, maxBytes int64) (NoFollowRead, error) {
	if err := ctx.Err(); err != nil {
		return NoFollowRead{}, err
	}
	parts, err := relComponents(relPath)
	if err != nil {
		return NoFollowRead{}, err
	}
	wd := strings.TrimSpace(l.workdir)
	if wd == "" {
		return NoFollowRead{}, ErrEmptyWorkdir
	}
	full := filepath.Join(append([]string{wd}, parts...)...)
	if len(l.denyPaths) > 0 && isPathDenied(full, l.denyPaths) {
		return NoFollowRead{}, fmt.Errorf("%w: %s", ErrDeniedPath, relPath)
	}
	info, stop, err := lstatChain(wd, parts)
	if err != nil {
		return NoFollowRead{}, err
	}
	if stop != nil {
		return NoFollowRead{LinkStat: *stop}, nil
	}
	if !info.Mode().IsRegular() {
		return NoFollowRead{LinkStat: LinkStat{Exists: true, IsDir: info.IsDir()}}, nil
	}
	if noFollowHooks.beforeOpen != nil {
		noFollowHooks.beforeOpen()
	}
	// openNoFollow refuses a final symlink (O_NOFOLLOW) and does not block on a FIFO
	// (O_NONBLOCK) where the platform has them.
	f, err := openNoFollow(full)
	if err != nil {
		// The path changed after the check (became a link, vanished, ...).
		return NoFollowRead{LinkStat: LinkStat{Exists: true}, Changed: true}, nil
	}
	defer f.Close()
	if noFollowHooks.afterOpen != nil {
		noFollowHooks.afterOpen()
	}
	opened, err := f.Stat()
	if err != nil {
		return NoFollowRead{}, err
	}
	// Re-check the whole path while the file is open, then require that it still names
	// the opened file: a link swapped in and out around the open is caught here.
	again, stop, err := lstatChain(wd, parts)
	if err != nil {
		return NoFollowRead{}, err
	}
	if stop != nil || !opened.Mode().IsRegular() || !again.Mode().IsRegular() || !os.SameFile(opened, again) {
		return NoFollowRead{LinkStat: LinkStat{Exists: true}, Changed: true}, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return NoFollowRead{}, err
	}
	out := NoFollowRead{LinkStat: LinkStat{Exists: true, IsRegular: true, Size: int64(len(data))}}
	if int64(len(data)) > maxBytes {
		out.TooLarge = true
		return out, nil
	}
	out.Data = data
	return out, nil
}

// ReadFileNoFollow implements NoFollowReader on a Linux host over the Transport. One
// shell command walks the path with `[ -L ]`, opens the file on fd 3, walks it again,
// requires that fd 3 and the path have the same device:inode, and then reads at most
// maxBytes+1 bytes from fd 3.
func (f *linuxFileAccess) ReadFileNoFollow(ctx context.Context, relPath string, maxBytes int64) (NoFollowRead, error) {
	if err := ctx.Err(); err != nil {
		return NoFollowRead{}, err
	}
	parts, err := relComponents(relPath)
	if err != nil {
		return NoFollowRead{}, err
	}
	wd := strings.TrimSpace(f.workdir)
	if wd == "" {
		return NoFollowRead{}, ErrEmptyWorkdir
	}
	if len(f.host.DenyPaths) > 0 && isPathDenied(joinRemotePath(wd, strings.Join(parts, "/")), f.host.DenyPaths) {
		return NoFollowRead{}, fmt.Errorf("%w: %s", ErrDeniedPath, relPath)
	}
	res, err := f.transport.Run(ctx, f.host, linuxNoFollowReadCommand(wd, parts, maxBytes))
	if err != nil {
		return NoFollowRead{}, err
	}
	if res.ExitCode != 0 {
		return NoFollowRead{}, fmt.Errorf("remote no-follow read failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return parseNoFollowRead(res.Stdout, maxBytes)
}

func linuxNoFollowReadCommand(wd string, parts []string, maxBytes int64) string {
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = shellQuote(p)
	}
	// chk <label-for-link> <label-for-missing>: walks the components without following.
	return fmt.Sprintf(
		`cd %s || exit 3; `+
			`chk() { p=''; for c in %s; do p="${p:+$p/}$c"; if [ -L "$p" ]; then printf '%%s\n' "$1"; exit 0; fi; if [ ! -e "$p" ]; then printf '%%s\n' "$2"; exit 0; fi; done; }; `+
			`chk LINK MISSING; `+
			`if [ -d "$p" ]; then printf 'DIR\n'; exit 0; fi; if [ ! -f "$p" ]; then printf 'OTHER\n'; exit 0; fi; `+
			`exec 3< "$p" || { printf 'CHANGED\n'; exit 0; }; `+
			`chk CHANGED CHANGED; `+
			`a=$(stat -L -c '%%d:%%i' /dev/fd/3) || exit 4; b=$(stat -c '%%d:%%i' -- "$p") || { printf 'CHANGED\n'; exit 0; }; `+
			`if [ ! -f /dev/fd/3 ] || [ -L "$p" ] || [ "$a" != "$b" ]; then printf 'CHANGED\n'; exit 0; fi; `+
			`printf 'FILE\n'; head -c %d <&3`,
		shellPath(wd), strings.Join(quoted, " "), maxBytes+1,
	)
}

// ReadFileNoFollow implements NoFollowReader on a Windows host. One PowerShell command
// walks the path refusing reparse points, opens the file with FileShare.Read (no other
// writer, no delete or rename of the file while it is open), walks the path again, and
// reads at most maxBytes+1 bytes from the open handle. Unlike Local and Linux it does not
// compare file IDs of the handle and the path (not verified on a real Windows host).
func (f *windowsFileAccess) ReadFileNoFollow(ctx context.Context, relPath string, maxBytes int64) (NoFollowRead, error) {
	if err := ctx.Err(); err != nil {
		return NoFollowRead{}, err
	}
	parts, err := relComponents(relPath)
	if err != nil {
		return NoFollowRead{}, err
	}
	wd := normalizeWindowsPath(f.workdir)
	if wd == "" {
		return NoFollowRead{}, ErrEmptyWorkdir
	}
	if len(f.host.DenyPaths) > 0 && isPathDenied(joinWindowsPath(wd, strings.Join(parts, `\`)), f.host.DenyPaths) {
		return NoFollowRead{}, fmt.Errorf("%w: %s", ErrDeniedPath, relPath)
	}
	res, err := f.transport.Run(ctx, f.host, wrapPowerShellCommand(windowsNoFollowReadScript(wd, parts, maxBytes)))
	if err != nil {
		return NoFollowRead{}, err
	}
	if res.ExitCode != 0 {
		return NoFollowRead{}, fmt.Errorf("remote windows no-follow read failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return parseNoFollowRead(res.Stdout, maxBytes)
}

func windowsNoFollowReadScript(wd string, parts []string, maxBytes int64) string {
	items := make([]string, len(parts))
	for i, p := range parts {
		items[i] = psSafeExpr(p)
	}
	return fmt.Sprintf(
		"[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; $parts = @(%s); "+
			"function Walk($link, $missing) { $p = %s; $i = $null; foreach ($c in $parts) { $p = Join-Path $p $c; "+
			"if (-not (Test-Path -LiteralPath $p)) { Write-Output $missing; exit 0 }; "+
			"$i = Get-Item -LiteralPath $p -Force; "+
			"if ($i.Attributes -band [System.IO.FileAttributes]::ReparsePoint) { Write-Output $link; exit 0 } }; return $i }; "+
			"$i = Walk 'LINK' 'MISSING'; if ($i.PSIsContainer) { Write-Output 'DIR'; exit 0 }; "+
			"try { $fs = [System.IO.File]::Open($i.FullName, 'Open', 'Read', 'Read') } catch { Write-Output 'CHANGED'; exit 0 }; "+
			"try { $j = Walk 'CHANGED' 'CHANGED'; "+
			"if ($j.PSIsContainer) { Write-Output 'CHANGED'; exit 0 }; "+
			"$buf = New-Object byte[] %d; $n = 0; while ($n -lt $buf.Length) { $r = $fs.Read($buf, $n, $buf.Length - $n); if ($r -le 0) { break }; $n += $r }; "+
			"[Console]::Write('FILE' + [char]10); [Console]::Write([System.Text.Encoding]::UTF8.GetString($buf, 0, $n)) } finally { $fs.Dispose() }",
		strings.Join(items, ", "), psSafeExpr(wd), maxBytes+1,
	)
}

// parseNoFollowRead parses "<STATUS>\n[data]" written by the remote commands.
func parseNoFollowRead(stdout string, maxBytes int64) (NoFollowRead, error) {
	status, data, _ := strings.Cut(stdout, "\n")
	switch strings.TrimSpace(status) {
	case "MISSING":
		return NoFollowRead{}, nil
	case "LINK":
		return NoFollowRead{LinkStat: LinkStat{Exists: true, Symlink: true}}, nil
	case "DIR":
		return NoFollowRead{LinkStat: LinkStat{Exists: true, IsDir: true}}, nil
	case "OTHER":
		return NoFollowRead{LinkStat: LinkStat{Exists: true}}, nil
	case "CHANGED":
		return NoFollowRead{LinkStat: LinkStat{Exists: true}, Changed: true}, nil
	case "FILE":
		out := NoFollowRead{LinkStat: LinkStat{Exists: true, IsRegular: true, Size: int64(len(data))}}
		if int64(len(data)) > maxBytes {
			out.TooLarge = true
			return out, nil
		}
		out.Data = []byte(data)
		return out, nil
	}
	return NoFollowRead{}, fmt.Errorf("unexpected remote no-follow read output: %q", strings.TrimSpace(status))
}

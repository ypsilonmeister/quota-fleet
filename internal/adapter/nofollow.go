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
// shell command (linuxNoFollowReadScript):
//
//   - enters each directory below the workdir with `cd -P` after a `[ -L ]` check, and
//     requires `env pwd -P` to be the expected physical path, so the directory the file
//     is read from is pinned (the shell's cwd) and no link was traversed. The external
//     pwd is used because a builtin `pwd -P` may return the path cached at the last cd
//     (dash) instead of calling getcwd;
//   - refuses a final link, a non-regular file and records the file's device, inode,
//     mode, size, mtime and ctime;
//   - opens and reads the file with `dd iflag=nofollow,nonblock`: the open itself refuses
//     a link (O_NOFOLLOW) and cannot block on a FIFO swapped in after the check
//     (O_NONBLOCK). At most maxBytes+1 bytes are read;
//   - checks again that the cwd is the expected path and that the file still has the
//     same device, inode, mode, size, mtime and ctime (a rename away and back changes
//     ctime), otherwise the result is CHANGED and the data is discarded.
//
// dd must support iflag=nofollow,nonblock (GNU coreutils and uutils do); otherwise the
// command fails with exit 5 instead of reading without the flags.
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
	return linuxNoFollowReadScript(wd, parts, maxBytes, linuxNoFollowTestHooks{})
}

// linuxNoFollowTestHooks are shell snippets that tests insert at the race points.
type linuxNoFollowTestHooks struct {
	beforeOpen string // after the checks, before dd opens the file
	afterRead  string // after dd read the file, before the final check
}

// noFollowEndMarker ends the output of a READ result: "READ\n<data>\n<marker> OK|CHANGED".
const noFollowEndMarker = "QF-NOFOLLOW-END"

func linuxNoFollowReadScript(wd string, parts []string, maxBytes int64, hooks linuxNoFollowTestHooks) string {
	dirs := make([]string, 0, len(parts)-1)
	for _, p := range parts[:len(parts)-1] {
		dirs = append(dirs, shellQuote(p))
	}
	dirList := strings.Join(dirs, " ")
	if dirList == "" {
		dirList = "''" // never iterated: see the [ -n ] guard
	}
	hook := func(s string) string {
		if s == "" {
			return ""
		}
		return s + "; "
	}
	const st = `stat -c '%d:%i:%f:%h:%s:%y:%z' -- "$n" 2>/dev/null`
	return fmt.Sprintf(
		`dd if=/dev/null iflag=nofollow,nonblock,fullblock bs=1 count=0 status=none 2>/dev/null || { echo 'dd lacks iflag=nofollow,nonblock' >&2; exit 5; }; `+
			`cd %s || exit 3; w=$(env pwd -P) || exit 3; e=${w%%/}; `+
			`for c in %s; do [ -n "$c" ] || continue; `+
			`if [ -L "$c" ]; then printf 'LINK\n'; exit 0; fi; `+
			`if [ ! -e "$c" ]; then printf 'MISSING\n'; exit 0; fi; `+
			`if [ ! -d "$c" ]; then printf 'OTHER\n'; exit 0; fi; `+
			`cd -P -- "$c" 2>/dev/null || { printf 'CHANGED\n'; exit 0; }; e="$e/$c"; `+
			`[ "$(env pwd -P)" = "$e" ] || { printf 'CHANGED\n'; exit 0; }; done; `+
			`n=%s; `+
			`if [ -L "$n" ]; then printf 'LINK\n'; exit 0; fi; `+
			`if [ ! -e "$n" ]; then printf 'MISSING\n'; exit 0; fi; `+
			`if [ -d "$n" ]; then printf 'DIR\n'; exit 0; fi; `+
			`if [ ! -f "$n" ]; then printf 'OTHER\n'; exit 0; fi; `+
			`a=$(%s) || { printf 'CHANGED\n'; exit 0; }; `+
			`%s`+
			`printf 'READ\n'; `+
			`if dd if="$n" iflag=nofollow,nonblock,fullblock bs=%d count=1 status=none 2>/dev/null; then `+
			`%s`+
			`b=$(%s) || b=; `+
			`if [ "$(env pwd -P)" = "$e" ] && [ ! -L "$n" ] && [ -f "$n" ] && [ -n "$b" ] && [ "$a" = "$b" ]; then printf '\n%s OK'; else printf '\n%s CHANGED'; fi; `+
			`else printf '\n%s CHANGED'; fi`,
		shellPath(wd), dirList, shellQuote(parts[len(parts)-1]), st,
		hook(hooks.beforeOpen), maxBytes+1, hook(hooks.afterRead), st,
		noFollowEndMarker, noFollowEndMarker, noFollowEndMarker,
	)
}

// ReadFileNoFollow implements NoFollowReader on a Windows host. One PowerShell command
// runs windowsNoFollowSource (C# through Add-Type), which uses CreateFileW directly:
//
//   - the workdir and every directory below it are opened and kept open without
//     FILE_SHARE_DELETE/FILE_SHARE_WRITE, so none of them can be renamed, deleted or
//     replaced while the file is read; each component is opened with
//     FILE_FLAG_OPEN_REPARSE_POINT, so the open never follows a final reparse point and
//     a component that is one is reported as LINK;
//   - the file is opened with FILE_FLAG_OPEN_REPARSE_POINT and FILE_SHARE_READ only; the
//     handle must not be a reparse point or a directory and must be a disk file
//     (GetFileType);
//   - at most maxBytes+1 bytes are read from that handle;
//   - the path is opened again and its volume serial number and file index must equal
//     those of the handle that was read (GetFileInformationByHandle).
//
// Not executed on a real Windows host in the tests (only the script text is checked).
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

// windowsNoFollowSource is compiled by Add-Type on the Windows host. QfNoFollow.Read
// returns MISSING, LINK, DIR, OTHER, CHANGED or FILE (then Data holds the bytes read).
const windowsNoFollowSource = `using System; using System.IO; using System.Collections.Generic; using System.Runtime.InteropServices; using Microsoft.Win32.SafeHandles; ` +
	`public static class QfNoFollow { ` +
	`[StructLayout(LayoutKind.Sequential)] public struct Bhfi { public uint Attr, C1, C2, A1, A2, W1, W2, Vol, SizeHigh, SizeLow, Links, IdxHigh, IdxLow; } ` +
	`[DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)] static extern SafeFileHandle CreateFileW(string name, uint access, uint share, IntPtr sa, uint disposition, uint flags, IntPtr template); ` +
	`[DllImport("kernel32.dll", SetLastError = true)] static extern bool GetFileInformationByHandle(SafeFileHandle h, out Bhfi info); ` +
	`[DllImport("kernel32.dll")] static extern uint GetFileType(SafeFileHandle h); ` +
	`const uint ReadAttributes = 0x80, ListOrReadData = 0x1, GenericRead = 0x80000000, ShareRead = 1, OpenExisting = 3; ` +
	`const uint OpenReparsePoint = 0x00200000, BackupSemantics = 0x02000000, AttrDirectory = 0x10, AttrReparsePoint = 0x400, FileTypeDisk = 1; ` +
	`public static byte[] Data; ` +
	`static SafeFileHandle Open(string p, uint access, uint flags, List<SafeFileHandle> keep, out int err) { ` +
	`SafeFileHandle h = CreateFileW(p, access, ShareRead, IntPtr.Zero, OpenExisting, flags, IntPtr.Zero); err = h.IsInvalid ? Marshal.GetLastWin32Error() : 0; if (!h.IsInvalid) { keep.Add(h); } return h; } ` +
	`static Bhfi Info(SafeFileHandle h) { Bhfi i; if (!GetFileInformationByHandle(h, out i)) { throw new IOException("GetFileInformationByHandle: " + Marshal.GetLastWin32Error()); } return i; } ` +
	`static bool Gone(int err) { return err == 2 || err == 3; } ` +
	`static bool Busy(int err) { return err == 5 || err == 32; } ` +
	`public static string Read(string wd, string[] parts, int max) { ` +
	`List<SafeFileHandle> keep = new List<SafeFileHandle>(); int err; ` +
	`try { ` +
	`if (Open(wd, ReadAttributes | ListOrReadData, BackupSemantics, keep, out err).IsInvalid) { throw new IOException("open workdir: " + err); } ` +
	`string p = wd; ` +
	`for (int k = 0; k < parts.Length - 1; k++) { p = Path.Combine(p, parts[k]); ` +
	`SafeFileHandle d = Open(p, ReadAttributes | ListOrReadData, OpenReparsePoint | BackupSemantics, keep, out err); ` +
	`if (d.IsInvalid) { if (Gone(err)) { return "MISSING"; } if (Busy(err)) { return "CHANGED"; } throw new IOException("open " + p + ": " + err); } ` +
	`Bhfi di = Info(d); if ((di.Attr & AttrReparsePoint) != 0) { return "LINK"; } if ((di.Attr & AttrDirectory) == 0) { return "OTHER"; } } ` +
	`p = Path.Combine(p, parts[parts.Length - 1]); ` +
	`SafeFileHandle f = Open(p, GenericRead, OpenReparsePoint | BackupSemantics, keep, out err); ` +
	`if (f.IsInvalid) { if (Gone(err)) { return "MISSING"; } if (Busy(err)) { return "CHANGED"; } throw new IOException("open " + p + ": " + err); } ` +
	`Bhfi fi = Info(f); if ((fi.Attr & AttrReparsePoint) != 0) { return "LINK"; } if ((fi.Attr & AttrDirectory) != 0) { return "DIR"; } if (GetFileType(f) != FileTypeDisk) { return "OTHER"; } ` +
	`byte[] buf = new byte[max + 1]; int n = 0; FileStream fs = new FileStream(f, FileAccess.Read, 1); ` +
	`while (n < buf.Length) { int r = fs.Read(buf, n, buf.Length - n); if (r <= 0) { break; } n += r; } ` +
	`SafeFileHandle again = Open(p, ReadAttributes, OpenReparsePoint | BackupSemantics, keep, out err); if (again.IsInvalid) { return "CHANGED"; } ` +
	`Bhfi ai = Info(again); ` +
	`if ((ai.Attr & (AttrReparsePoint | AttrDirectory)) != 0 || ai.Vol != fi.Vol || ai.IdxHigh != fi.IdxHigh || ai.IdxLow != fi.IdxLow) { return "CHANGED"; } ` +
	`Data = new byte[n]; Array.Copy(buf, Data, n); return "FILE"; ` +
	`} finally { for (int k = keep.Count - 1; k >= 0; k--) { keep[k].Dispose(); } } } }`

func windowsNoFollowReadScript(wd string, parts []string, maxBytes int64) string {
	items := make([]string, len(parts))
	for i, p := range parts {
		items[i] = psSafeExpr(p)
	}
	return fmt.Sprintf(
		"[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; "+
			"try { Add-Type -TypeDefinition %s; $s = [QfNoFollow]::Read(%s, [string[]]@(%s), %d) } catch { [Console]::Error.WriteLine($_.Exception.Message); exit 4 }; "+
			"if ($s -eq 'FILE') { [Console]::Write('FILE' + [char]10); [Console]::Write([System.Text.Encoding]::UTF8.GetString([QfNoFollow]::Data)) } else { Write-Output $s }",
		psSafeExpr(windowsNoFollowSource), psSafeExpr(wd), strings.Join(items, ", "), maxBytes,
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
	case "READ":
		// Linux: "READ\n<data>\n<marker> OK|CHANGED"; the marker line is always last.
		i := strings.LastIndex(data, "\n"+noFollowEndMarker+" ")
		if i < 0 {
			return NoFollowRead{}, fmt.Errorf("remote no-follow read output lacks the end marker")
		}
		switch strings.TrimSpace(data[i+len(noFollowEndMarker)+2:]) {
		case "OK":
		case "CHANGED":
			return NoFollowRead{LinkStat: LinkStat{Exists: true}, Changed: true}, nil
		default:
			return NoFollowRead{}, fmt.Errorf("unexpected remote no-follow read end marker: %q", data[i+1:])
		}
		data = data[:i]
		out := NoFollowRead{LinkStat: LinkStat{Exists: true, IsRegular: true, Size: int64(len(data))}}
		if int64(len(data)) > maxBytes {
			out.TooLarge = true
			return out, nil
		}
		out.Data = []byte(data)
		return out, nil
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

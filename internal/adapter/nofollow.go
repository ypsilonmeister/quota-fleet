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
//     (dash) instead of calling getcwd. The workdir itself is the trusted starting point
//     (configured by the operator and given by the chain definition), not checked;
//   - then runs one python3 process (perl when python3 is missing) on the final
//     component, which does everything on one file descriptor:
//     lstat the name and refuse a link, a directory and a non-regular file;
//     open it with O_RDONLY|O_NOFOLLOW|O_NONBLOCK (the open refuses a link swapped in
//     after the lstat and does not block on a FIFO);
//     fstat that descriptor and require a regular file with the device and inode of the
//     lstat, before reading anything;
//     read at most maxBytes+1 bytes from that same descriptor into memory;
//     fstat it again (mode, links, size, mtime, ctime must be unchanged), lstat the name
//     (same device and inode, still a regular file) and require the cwd to still be the
//     expected path.
//     Only after all checks pass are the bytes written (READ framing); otherwise the
//     result is CHANGED and no byte of the file is written.
//
// If neither python3 nor perl exists the command fails with exit 5 (the read is not
// attempted without the checks).
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

// linuxNoFollowTestHooks are shell snippets that tests run at the race points of the
// final-component reader. interp forces "python3" or "perl" (empty: python3, else perl).
type linuxNoFollowTestHooks struct {
	beforeOpen string // after the lstat checks, before the open
	afterRead  string // after the read, before the final checks
	interp     string
}

// noFollowEndMarker ends the output of a READ result: "READ\n<data>\n<marker> OK".
const noFollowEndMarker = "QF-NOFOLLOW-END"

// linuxNoFollowPython is the final-component reader for python3. Arguments: name,
// maxBytes, expected cwd, then the optional test hooks (before open, after read).
// Statuses are written only through done, which exits; file bytes are written only
// after every check passed.
const linuxNoFollowPython = `import os, sys, stat
n, mx, e = sys.argv[1], int(sys.argv[2]), sys.argv[3]
h = sys.argv[4:] + ["", ""]
o = sys.stdout.buffer
def done(s):
    o.write(s); o.flush(); os._exit(0)
try:
    l = os.lstat(n)
except FileNotFoundError:
    done(b"MISSING\n")
except OSError:
    done(b"CHANGED\n")
if stat.S_ISLNK(l.st_mode): done(b"LINK\n")
if stat.S_ISDIR(l.st_mode): done(b"DIR\n")
if not stat.S_ISREG(l.st_mode): done(b"OTHER\n")
if h[0]: os.system(h[0])
try:
    fd = os.open(n, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
except OSError:
    done(b"CHANGED\n")
def sig(s): return (s.st_dev, s.st_ino, s.st_mode, s.st_nlink, s.st_size, s.st_mtime_ns, s.st_ctime_ns)
f = os.fstat(fd)
if not stat.S_ISREG(f.st_mode) or (f.st_dev, f.st_ino) != (l.st_dev, l.st_ino): done(b"CHANGED\n")
buf = b""
try:
    while len(buf) <= mx:
        r = os.read(fd, mx + 1 - len(buf))
        if not r: break
        buf += r
except OSError:
    done(b"CHANGED\n")
if h[1]: os.system(h[1])
try:
    g = os.fstat(fd); a = os.lstat(n); c = os.getcwd()
except OSError:
    done(b"CHANGED\n")
if sig(g) != sig(f) or not stat.S_ISREG(a.st_mode) or (a.st_dev, a.st_ino) != (f.st_dev, f.st_ino) or c != e: done(b"CHANGED\n")
done(b"READ\n" + buf + b"\n` + noFollowEndMarker + ` OK")
`

// linuxNoFollowPerl is the same reader for perl (Fcntl and Cwd are in perl-base).
// Time::HiRes, when present, gives sub-second mtime and ctime for the final comparison.
const linuxNoFollowPerl = `use strict; use Fcntl qw(O_RDONLY O_NOFOLLOW O_NONBLOCK S_ISREG S_ISDIR S_ISLNK); use Cwd qw(getcwd);
BEGIN { eval { require Time::HiRes; Time::HiRes->import(qw(stat lstat)); 1 } }
my ($n, $mx, $e, @h) = @ARGV; push @h, "", "";
binmode STDOUT;
sub done { print STDOUT $_[0]; close STDOUT; exit 0 }
my @l = lstat($n); done($!{ENOENT} ? "MISSING\n" : "CHANGED\n") unless @l;
done("LINK\n") if S_ISLNK($l[2]); done("DIR\n") if S_ISDIR($l[2]); done("OTHER\n") unless S_ISREG($l[2]);
system($h[0]) if length $h[0];
sysopen(my $fh, $n, O_RDONLY | O_NOFOLLOW | O_NONBLOCK) or done("CHANGED\n");
my @f = stat($fh);
done("CHANGED\n") unless @f && S_ISREG($f[2]) && $f[0] == $l[0] && $f[1] == $l[1];
my $buf = "";
while (length($buf) <= $mx) { my $r = sysread($fh, $buf, $mx + 1 - length($buf), length($buf)); done("CHANGED\n") unless defined $r; last if $r == 0; }
system($h[1]) if length $h[1];
my @g = stat($fh); my @a = lstat($n); my $c = getcwd();
done("CHANGED\n") unless @g && @a && defined $c && S_ISREG($a[2]) && !S_ISLNK($a[2]) && $a[0] == $f[0] && $a[1] == $f[1] && $c eq $e
  && join(":", @g[0, 1, 2, 3, 7, 9, 10]) eq join(":", @f[0, 1, 2, 3, 7, 9, 10]);
done("READ\n" . $buf . "\n` + noFollowEndMarker + ` OK");
`

func linuxNoFollowReadScript(wd string, parts []string, maxBytes int64, hooks linuxNoFollowTestHooks) string {
	dirs := make([]string, 0, len(parts)-1)
	for _, p := range parts[:len(parts)-1] {
		dirs = append(dirs, shellQuote(p))
	}
	dirList := strings.Join(dirs, " ")
	if dirList == "" {
		dirList = "''" // never iterated: see the [ -n ] guard
	}
	args := `"$n" ` + fmt.Sprint(maxBytes) + ` "$e"`
	if hooks.beforeOpen != "" || hooks.afterRead != "" {
		args += " " + shellQuote(hooks.beforeOpen) + " " + shellQuote(hooks.afterRead)
	}
	py := `python3 -c ` + shellQuote(linuxNoFollowPython) + ` ` + args
	pl := `perl -e ` + shellQuote(linuxNoFollowPerl) + ` -- ` + args
	var reader string
	switch hooks.interp {
	case "python3":
		reader = py
	case "perl":
		reader = pl
	default:
		reader = `if command -v python3 >/dev/null 2>&1; then ` + py + `; ` +
			`elif command -v perl >/dev/null 2>&1; then ` + pl + `; ` +
			`else echo 'no-follow read needs python3 or perl' >&2; exit 5; fi`
	}
	return fmt.Sprintf(
		`cd %s || exit 3; w=$(env pwd -P) || exit 3; e=${w%%/}; `+
			`for c in %s; do [ -n "$c" ] || continue; `+
			`if [ -L "$c" ]; then printf 'LINK\n'; exit 0; fi; `+
			`if [ ! -e "$c" ]; then printf 'MISSING\n'; exit 0; fi; `+
			`if [ ! -d "$c" ]; then printf 'OTHER\n'; exit 0; fi; `+
			`cd -P -- "$c" 2>/dev/null || { printf 'CHANGED\n'; exit 0; }; e="$e/$c"; `+
			`[ "$(env pwd -P)" = "$e" ] || { printf 'CHANGED\n'; exit 0; }; done; `+
			`[ -n "$e" ] || e=/; n=%s; %s`,
		shellPath(wd), dirList, shellQuote(parts[len(parts)-1]), reader,
	)
}

// ReadFileNoFollow implements NoFollowReader on a Windows host. One PowerShell command
// runs windowsNoFollowSource (C# through Add-Type), which uses CreateFileW directly:
//
//   - the workdir is the trusted starting point, as in the local and Linux
//     implementations: it is opened (and kept open) without FILE_FLAG_OPEN_REPARSE_POINT
//     and its attributes are not checked, so a workdir that is itself a symbolic link or
//     junction is followed, as are reparse points in its ancestors. The workdir path is
//     operator configuration (host / chain definition); the agent writes below it;
//   - every component below the workdir (the directories and the file) is opened with
//     FILE_FLAG_OPEN_REPARSE_POINT, so the open never follows a final reparse point and
//     a component that is one is reported as LINK. These directories are kept open
//     without FILE_SHARE_DELETE/FILE_SHARE_WRITE, so none of them can be renamed,
//     deleted or replaced while the file is read;
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

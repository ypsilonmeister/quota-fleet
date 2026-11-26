//go:build unix

package adapter

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const nfRel = ".qf-chain/run/2/verdict.json"

func nfSetup(t *testing.T) (wd, outside string) {
	t.Helper()
	root := t.TempDir()
	wd = filepath.Join(root, "wd")
	outside = filepath.Join(root, "outside")
	for _, d := range []string{filepath.Join(wd, ".qf-chain/run/2"), filepath.Join(outside, "2")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wd, nfRel), []byte(`{"verdict":"inside"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "2", "verdict.json"), []byte(`{"verdict":"outside"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return wd, outside
}

func setNoFollowHooks(t *testing.T, before, after func()) {
	noFollowHooks.beforeOpen, noFollowHooks.afterOpen = before, after
	t.Cleanup(func() { noFollowHooks.beforeOpen, noFollowHooks.afterOpen = nil, nil })
}

// TestReadFileNoFollowLocal fixes docs/22 §7.1 for the local file access: the link check
// and the read are one operation, and the read itself is bounded.
func TestReadFileNoFollowLocal(t *testing.T) {
	ctx := context.Background()
	read := func(t *testing.T, wd string) NoFollowRead {
		t.Helper()
		rd, err := NewLocalFileAccess(wd).ReadFileNoFollow(ctx, nfRel, 64)
		if err != nil {
			t.Fatal(err)
		}
		return rd
	}

	t.Run("regular file", func(t *testing.T) {
		wd, _ := nfSetup(t)
		if rd := read(t, wd); rd.Changed || !rd.IsRegular || string(rd.Data) != `{"verdict":"inside"}` {
			t.Fatalf("%+v", rd)
		}
	})
	t.Run("read is bounded", func(t *testing.T) {
		wd, _ := nfSetup(t)
		if err := os.WriteFile(filepath.Join(wd, nfRel), []byte(strings.Repeat("a", 1<<20)), 0o644); err != nil {
			t.Fatal(err)
		}
		rd := read(t, wd)
		if !rd.TooLarge || rd.Data != nil || rd.Size != 65 {
			t.Fatalf("TooLarge=%v len=%d size=%d", rd.TooLarge, len(rd.Data), rd.Size)
		}
	})
	t.Run("static link and FIFO", func(t *testing.T) {
		wd, outside := nfSetup(t)
		p := filepath.Join(wd, nfRel)
		_ = os.Remove(p)
		if err := os.Symlink(filepath.Join(outside, "2", "verdict.json"), p); err != nil {
			t.Fatal(err)
		}
		if rd := read(t, wd); !rd.Symlink || rd.Data != nil {
			t.Fatalf("file link: %+v", rd)
		}
		_ = os.Remove(p)
		if err := syscall.Mkfifo(p, 0o644); err != nil {
			t.Fatal(err)
		}
		if rd := read(t, wd); rd.IsRegular || rd.Data != nil {
			t.Fatalf("fifo: %+v", rd)
		}
	})
	t.Run("file replaced by a link between check and open", func(t *testing.T) {
		wd, outside := nfSetup(t)
		p := filepath.Join(wd, nfRel)
		setNoFollowHooks(t, func() {
			_ = os.Remove(p)
			_ = os.Symlink(filepath.Join(outside, "2", "verdict.json"), p)
		}, nil)
		if rd := read(t, wd); !rd.Changed || rd.Data != nil {
			t.Fatalf("%+v", rd)
		}
	})
	t.Run("parent swapped to a link and back around the open", func(t *testing.T) {
		wd, outside := nfSetup(t)
		parent := filepath.Join(wd, ".qf-chain/run")
		saved := filepath.Join(wd, ".qf-chain/run.saved")
		setNoFollowHooks(t, func() {
			_ = os.Rename(parent, saved)
			_ = os.Symlink(outside, parent)
		}, func() {
			_ = os.Remove(parent)
			_ = os.Rename(saved, parent)
		})
		if rd := read(t, wd); !rd.Changed || rd.Data != nil {
			t.Fatalf("ABA swap not detected: %+v data=%q", rd, rd.Data)
		}
	})
	t.Run("parent left as a link after the open", func(t *testing.T) {
		wd, outside := nfSetup(t)
		parent := filepath.Join(wd, ".qf-chain/run")
		setNoFollowHooks(t, func() {
			_ = os.RemoveAll(parent)
			_ = os.Symlink(outside, parent)
		}, nil)
		if rd := read(t, wd); !rd.Changed || rd.Data != nil {
			t.Fatalf("%+v", rd)
		}
	})
	t.Run("file replaced by another regular file after the open", func(t *testing.T) {
		wd, _ := nfSetup(t)
		p := filepath.Join(wd, nfRel)
		setNoFollowHooks(t, nil, func() {
			tmp := p + ".new"
			_ = os.WriteFile(tmp, []byte(`{"verdict":"swapped"}`), 0o644)
			_ = os.Rename(tmp, p)
		})
		if rd := read(t, wd); !rd.Changed || rd.Data != nil {
			t.Fatalf("%+v", rd)
		}
	})
}

// TestLinuxNoFollowReadCommand runs the remote Linux command with the local sh.
func TestLinuxNoFollowReadCommand(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	parts := strings.Split(nfRel, "/")
	run := func(t *testing.T, wd string) NoFollowRead {
		t.Helper()
		out, err := exec.Command("sh", "-c", linuxNoFollowReadCommand(wd, parts, 64)).Output()
		if err != nil {
			t.Fatalf("sh: %v", err)
		}
		rd, err := parseNoFollowRead(string(out), 64)
		if err != nil {
			t.Fatal(err)
		}
		return rd
	}
	wd, outside := nfSetup(t)
	if rd := run(t, wd); !rd.IsRegular || string(rd.Data) != `{"verdict":"inside"}` {
		t.Fatalf("regular: %+v", rd)
	}
	p := filepath.Join(wd, nfRel)
	if err := os.WriteFile(p, []byte(strings.Repeat("a", 1<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	if rd := run(t, wd); !rd.TooLarge || rd.Size != 65 {
		t.Fatalf("large: TooLarge=%v size=%d", rd.TooLarge, rd.Size)
	}
	_ = os.Remove(p)
	if rd := run(t, wd); rd.Exists {
		t.Fatalf("missing: %+v", rd)
	}
	if err := os.Symlink(filepath.Join(outside, "2", "verdict.json"), p); err != nil {
		t.Fatal(err)
	}
	if rd := run(t, wd); !rd.Symlink {
		t.Fatalf("file link: %+v", rd)
	}
	parent := filepath.Join(wd, ".qf-chain/run")
	_ = os.RemoveAll(parent)
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	if rd := run(t, wd); !rd.Symlink || rd.Data != nil {
		t.Fatalf("parent link: %+v", rd)
	}
}

// TestLinuxNoFollowReadRaces runs the remote Linux command with the local sh and changes
// the file at the race points of the command (shell snippets inserted by the test). The
// open itself refuses a link and does not block on a FIFO, and a file renamed away and
// back or replaced during the read is reported as CHANGED.
func TestLinuxNoFollowReadRaces(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	parts := strings.Split(nfRel, "/")
	run := func(t *testing.T, wd string, hooks linuxNoFollowTestHooks) NoFollowRead {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", linuxNoFollowReadScript(wd, parts, 64, hooks))
		cmd.Dir = t.TempDir()
		cmd.WaitDelay = time.Second // a blocked dd child keeps stdout open
		out, err := cmd.Output()
		if ctx.Err() != nil {
			t.Fatalf("command blocked: %v", ctx.Err())
		}
		if err != nil {
			t.Fatalf("sh: %v", err)
		}
		rd, err := parseNoFollowRead(string(out), 64)
		if err != nil {
			t.Fatalf("parse %q: %v", out, err)
		}
		return rd
	}
	q := shellQuote

	t.Run("FIFO swapped in after the check does not block", func(t *testing.T) {
		wd, _ := nfSetup(t)
		p := filepath.Join(wd, nfRel)
		rd := run(t, wd, linuxNoFollowTestHooks{beforeOpen: "rm -f " + q(p) + " && mkfifo " + q(p)})
		if !rd.Changed || rd.Data != nil {
			t.Fatalf("%+v", rd)
		}
	})
	t.Run("FIFO swapped in and the file renamed back after the read", func(t *testing.T) {
		wd, _ := nfSetup(t)
		p := filepath.Join(wd, nfRel)
		rd := run(t, wd, linuxNoFollowTestHooks{
			beforeOpen: "mv " + q(p) + " " + q(p+".saved") + " && mkfifo " + q(p),
			afterRead:  "rm -f " + q(p) + " && mv " + q(p+".saved") + " " + q(p),
		})
		if !rd.Changed || rd.Data != nil {
			t.Fatalf("ABA via FIFO not detected: %+v", rd)
		}
	})
	t.Run("link swapped in after the check is refused by the open", func(t *testing.T) {
		wd, outside := nfSetup(t)
		p := filepath.Join(wd, nfRel)
		rd := run(t, wd, linuxNoFollowTestHooks{beforeOpen: "rm -f " + q(p) + " && ln -s " + q(filepath.Join(outside, "2", "verdict.json")) + " " + q(p)})
		if !rd.Changed || rd.Data != nil {
			t.Fatalf("%+v", rd)
		}
	})
	t.Run("another regular file renamed in and back around the read", func(t *testing.T) {
		wd, _ := nfSetup(t)
		p := filepath.Join(wd, nfRel)
		rd := run(t, wd, linuxNoFollowTestHooks{
			beforeOpen: "mv " + q(p) + " " + q(p+".saved") + " && printf swapped > " + q(p),
			afterRead:  "mv " + q(p+".saved") + " " + q(p),
		})
		if !rd.Changed || rd.Data != nil {
			t.Fatalf("swap not detected: %+v data=%q", rd, rd.Data)
		}
	})
	t.Run("file renamed away and back during the read", func(t *testing.T) {
		wd, _ := nfSetup(t)
		p := filepath.Join(wd, nfRel)
		rd := run(t, wd, linuxNoFollowTestHooks{afterRead: "mv " + q(p) + " " + q(p+".x") + " && mv " + q(p+".x") + " " + q(p)})
		if !rd.Changed || rd.Data != nil {
			t.Fatalf("%+v", rd)
		}
	})
	t.Run("parent swapped to a link and back: the pinned directory is read", func(t *testing.T) {
		wd, outside := nfSetup(t)
		parent := filepath.Join(wd, ".qf-chain/run")
		rd := run(t, wd, linuxNoFollowTestHooks{
			beforeOpen: "mv " + q(parent) + " " + q(parent+".saved") + " && ln -s " + q(outside) + " " + q(parent),
			afterRead:  "rm -f " + q(parent) + " && mv " + q(parent+".saved") + " " + q(parent),
		})
		if strings.Contains(string(rd.Data), "outside") {
			t.Fatalf("read through the swapped link: %q", rd.Data)
		}
		if !rd.Changed && string(rd.Data) != `{"verdict":"inside"}` {
			t.Fatalf("%+v data=%q", rd, rd.Data)
		}
	})
	t.Run("parent left as a link after the read", func(t *testing.T) {
		wd, outside := nfSetup(t)
		parent := filepath.Join(wd, ".qf-chain/run")
		rd := run(t, wd, linuxNoFollowTestHooks{
			beforeOpen: "mv " + q(parent) + " " + q(parent+".saved") + " && ln -s " + q(outside) + " " + q(parent),
		})
		if !rd.Changed || rd.Data != nil {
			t.Fatalf("%+v", rd)
		}
	})
	t.Run("static FIFO is not opened", func(t *testing.T) {
		wd, _ := nfSetup(t)
		p := filepath.Join(wd, nfRel)
		_ = os.Remove(p)
		if err := syscall.Mkfifo(p, 0o644); err != nil {
			t.Fatal(err)
		}
		if rd := run(t, wd, linuxNoFollowTestHooks{}); rd.IsRegular || rd.Data != nil || !rd.Exists {
			t.Fatalf("%+v", rd)
		}
	})
	t.Run("data ending in newlines and NUL bytes is kept", func(t *testing.T) {
		wd, _ := nfSetup(t)
		want := "{\"v\":1}\n\x00\n\n"
		if err := os.WriteFile(filepath.Join(wd, nfRel), []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		if rd := run(t, wd, linuxNoFollowTestHooks{}); string(rd.Data) != want {
			t.Fatalf("data %q", rd.Data)
		}
	})
}

func TestParseNoFollowReadEndMarker(t *testing.T) {
	if _, err := parseNoFollowRead("READ\n{}", 64); err == nil {
		t.Fatal("output without the end marker accepted")
	}
	rd, err := parseNoFollowRead("READ\nabc\n"+noFollowEndMarker+" CHANGED", 64)
	if err != nil || !rd.Changed || rd.Data != nil {
		t.Fatalf("%+v %v", rd, err)
	}
	rd, err = parseNoFollowRead("READ\nabc\n"+noFollowEndMarker+" OK", 64)
	if err != nil || string(rd.Data) != "abc" {
		t.Fatalf("%+v %v", rd, err)
	}
}

func TestWindowsNoFollowReadScript(t *testing.T) {
	// The script is wrapped in -Command "..."; a double quote would end it early.
	s := windowsNoFollowReadScript(`C:\w`, strings.Split(nfRel, "/"), 4096)
	if strings.Contains(s, `"`) || strings.ContainsAny(s, "\r\n") {
		t.Fatalf("double quote or newline in script: %s", s)
	}
	if n := len(wrapPowerShellCommand(s)); n > 8000 {
		t.Fatalf("command is %d bytes; cmd.exe accepts at most 8191", n)
	}
	for _, want := range []string{
		"4096", "new byte[max + 1]", // bound
		"OpenReparsePoint = 0x00200000",                      // the open does not follow a reparse point
		"CreateFileW(p, access, ShareRead,",                  // no FILE_SHARE_DELETE/WRITE: pinned path
		"GetFileType(f) != FileTypeDisk",                     // only disk files
		"ai.IdxHigh != fi.IdxHigh || ai.IdxLow != fi.IdxLow", // handle and path identity
	} {
		if !strings.Contains(windowsNoFollowSource, want) && !strings.Contains(s, want) {
			t.Fatalf("script lacks %q", want)
		}
	}
	// The C# source is embedded as a PowerShell expression; decoding it must give back
	// the source exactly (double quotes are written as [char]34).
	expr := psSafeExpr(windowsNoFollowSource)
	decoded := strings.NewReplacer("' + [char]34 + '", `"`).Replace(expr)
	decoded = strings.TrimSuffix(strings.TrimPrefix(decoded, "('"), "')")
	decoded = strings.ReplaceAll(decoded, "''", "'")
	if decoded != windowsNoFollowSource {
		t.Fatalf("C# source not preserved:\n%s", decoded)
	}
}

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

func TestWindowsNoFollowReadScriptHasNoDoubleQuotes(t *testing.T) {
	// The script is wrapped in -Command "..."; a double quote would end it early.
	s := windowsNoFollowReadScript(`C:\w`, strings.Split(nfRel, "/"), 4096)
	if strings.Contains(s, `"`) {
		t.Fatalf("double quote in script: %s", s)
	}
	if !strings.Contains(s, "4097") || !strings.Contains(s, "'Read', 'Read'") {
		t.Fatalf("script lacks the bound or the share mode: %s", s)
	}
}

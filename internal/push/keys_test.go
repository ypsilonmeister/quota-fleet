package push

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVAPIDKeysCreatedOnceAndReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", KeyFileName)
	first, created, err := LoadOrCreateVAPIDKeys(path)
	if err != nil || !created {
		t.Fatalf("create = %v, %v", created, err)
	}
	public, err := base64.RawURLEncoding.DecodeString(first.PublicKey)
	if err != nil || len(public) != 65 || public[0] != 4 {
		t.Errorf("public key = %d bytes, %v", len(public), err)
	}
	assertMode0600(t, path)
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Errorf("state dir holds %v (%v), want only the key file", entries, err)
	}

	again, created, err := LoadOrCreateVAPIDKeys(path)
	if err != nil || created {
		t.Fatalf("reload = %v, %v", created, err)
	}
	if again != first {
		t.Errorf("keys changed on reload")
	}

	// A mode widened by a copy or restore is narrowed again.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if again, _, err = LoadOrCreateVAPIDKeys(path); err != nil || again != first {
		t.Fatalf("reload after chmod: %v", err)
	}
	assertMode0600(t, path)
}

func assertMode0600(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestVAPIDKeysUnusableFileIsKept(t *testing.T) {
	dir := t.TempDir()
	one, _, err := LoadOrCreateVAPIDKeys(filepath.Join(dir, "one.json"))
	if err != nil {
		t.Fatal(err)
	}
	two, _, err := LoadOrCreateVAPIDKeys(filepath.Join(dir, "two.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"empty":      "",
		"truncated":  `{"public_key":"` + one.PublicKey + `","private_key":"` + one.PrivateKey,
		"no private": `{"public_key":"` + one.PublicKey + `"}`,
		"mismatch":   `{"public_key":"` + one.PublicKey + `","private_key":"` + two.PrivateKey + `"}`,
	} {
		path := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, created, err := LoadOrCreateVAPIDKeys(path)
		if err == nil || created {
			t.Errorf("%s: created=%v err=%v, want an error", name, created, err)
			continue
		}
		for _, secret := range []string{one.PrivateKey, two.PrivateKey} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error contains the private key", name)
			}
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != content {
			t.Errorf("%s: file was changed", name)
		}
	}
	if _, _, err := LoadOrCreateVAPIDKeys(dir); err == nil {
		t.Errorf("a directory was accepted as the key file")
	}
}

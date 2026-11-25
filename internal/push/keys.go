// Package push sends Web Push notifications when jobs finish (docs/16).
package push

import (
	"bytes"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// KeyFileName is the file under state_dir that holds the VAPID key pair.
const KeyFileName = "vapid.json"

// VAPIDKeys is the application server key pair, both halves base64url without padding.
// PrivateKey must never leave the server.
type VAPIDKeys struct {
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
}

// LoadOrCreateVAPIDKeys reads the key pair at path, generating and saving one with mode
// 0600 if the file does not exist. A file that cannot be used is an error and is left as
// it is: a new key pair would silently invalidate every existing subscription.
func LoadOrCreateVAPIDKeys(path string) (keys VAPIDKeys, created bool, err error) {
	keys, err = loadVAPIDKeys(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return keys, false, err
	}
	private, public, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return VAPIDKeys{}, false, fmt.Errorf("generate VAPID keys: %w", err)
	}
	keys = VAPIDKeys{PublicKey: public, PrivateKey: private}
	if err := keys.validate(); err != nil {
		return VAPIDKeys{}, false, fmt.Errorf("generate VAPID keys: %w", err)
	}
	if err := saveVAPIDKeys(path, keys); err != nil {
		return VAPIDKeys{}, false, fmt.Errorf("save VAPID keys to %q: %w", path, err)
	}
	return keys, true, nil
}

func loadVAPIDKeys(path string) (VAPIDKeys, error) {
	info, err := os.Stat(path)
	if err != nil {
		return VAPIDKeys{}, fmt.Errorf("read VAPID key file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return VAPIDKeys{}, fmt.Errorf("VAPID key file %q is not a regular file", path)
	}
	// Restored backups and manual copies often lose the mode.
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return VAPIDKeys{}, fmt.Errorf("restrict VAPID key file %q to mode 0600: %w", path, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return VAPIDKeys{}, fmt.Errorf("read VAPID key file: %w", err)
	}
	var keys VAPIDKeys
	if err := json.Unmarshal(data, &keys); err != nil {
		// The decoder's message may quote the content, which includes the private key.
		return VAPIDKeys{}, fmt.Errorf("VAPID key file %q is not valid JSON", path)
	}
	if err := keys.validate(); err != nil {
		return VAPIDKeys{}, fmt.Errorf("VAPID key file %q: %w", path, err)
	}
	return keys, nil
}

// validate checks that both keys are well formed P-256 keys belonging together.
func (k VAPIDKeys) validate() error {
	private, err := base64.RawURLEncoding.DecodeString(k.PrivateKey)
	if err != nil {
		return errors.New("private_key is not unpadded base64url")
	}
	key, err := ecdh.P256().NewPrivateKey(private)
	if err != nil {
		return errors.New("private_key is not a P-256 private key")
	}
	public, err := base64.RawURLEncoding.DecodeString(k.PublicKey)
	if err != nil || !bytes.Equal(public, key.PublicKey().Bytes()) {
		return errors.New("public_key does not match private_key")
	}
	return nil
}

// saveVAPIDKeys writes the file completely or not at all.
func saveVAPIDKeys(path string, keys VAPIDKeys) (err error) {
	data, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// CreateTemp creates the file with mode 0600.
	f, err := os.CreateTemp(dir, KeyFileName+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

package push

import (
	"crypto/ecdh"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const (
	maxEndpointBytes  = 2048
	maxUserAgentRunes = 512
	// authSecretBytes is the length of the authentication secret (RFC 8291).
	authSecretBytes = 16
)

// NewSubscription validates what a browser sent and returns the subscription to store.
// Errors wrap store.ErrInvalidInput and never contain the endpoint or the keys.
func NewSubscription(endpoint, p256dh, auth, userAgent string) (store.PushSubscription, error) {
	invalid := func(reason string) (store.PushSubscription, error) {
		return store.PushSubscription{}, fmt.Errorf("%w: %s", store.ErrInvalidInput, reason)
	}
	if len(endpoint) > maxEndpointBytes {
		return invalid("endpoint is too long")
	}
	// Push services are HTTPS only (RFC 8030).
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return invalid("endpoint must be an https URL without user information")
	}
	publicKey, err := decodeKey(p256dh)
	if err == nil {
		_, err = ecdh.P256().NewPublicKey(publicKey)
	}
	if err != nil {
		return invalid("keys.p256dh must be a base64url uncompressed P-256 public key")
	}
	secret, err := decodeKey(auth)
	if err != nil || len(secret) != authSecretBytes {
		return invalid("keys.auth must be a base64url 16 byte secret")
	}
	if runes := []rune(userAgent); len(runes) > maxUserAgentRunes {
		userAgent = string(runes[:maxUserAgentRunes])
	}
	return store.PushSubscription{
		Endpoint:  endpoint,
		P256dh:    base64.RawURLEncoding.EncodeToString(publicKey),
		Auth:      base64.RawURLEncoding.EncodeToString(secret),
		UserAgent: userAgent,
	}, nil
}

// decodeKey accepts base64url or standard base64, with or without padding.
func decodeKey(key string) ([]byte, error) {
	key = strings.TrimRight(key, "=")
	if key == "" {
		return nil, errors.New("empty key")
	}
	if data, err := base64.RawURLEncoding.DecodeString(key); err == nil {
		return data, nil
	}
	return base64.RawStdEncoding.DecodeString(key)
}

// Package auth issues and verifies API keys.
//
// There is deliberately no "authentication disabled" mode. A service that can
// run programs on a developer's machine must never be reachable unauthenticated
// by accident, so on first run the server generates a key, prints it once, and
// stores only its hash.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Errors callers distinguish.
var (
	ErrUnauthorized = errors.New("auth: invalid credentials")
	ErrMalformed    = errors.New("auth: malformed token")
)

const (
	// prefix marks a NeelKIT key so a leaked string is recognisable in a log
	// and can be revoked without guessing what it is.
	prefix = "nk"
	// saltLen is the per-key salt, which keeps identical secrets from producing
	// identical hashes and makes a precomputed table useless.
	saltLen = 16
)

// Key is a stored credential. The secret itself is never persisted.
type Key struct {
	ID         string
	UserID     string
	Name       string
	Hash       string
	Salt       string
	CreatedAt  int64
	LastUsedAt int64
	RevokedAt  int64
}

// Generate creates a new key, returning the token to show once and the record
// to store.
func Generate(userID, name string) (token string, k Key, err error) {
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", Key{}, fmt.Errorf("auth: entropy unavailable: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", Key{}, fmt.Errorf("auth: entropy unavailable: %w", err)
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", Key{}, fmt.Errorf("auth: entropy unavailable: %w", err)
	}

	keyID := hex.EncodeToString(idBytes)
	secretStr := base64.RawURLEncoding.EncodeToString(secret)

	hash, err := derive(secretStr, salt)
	if err != nil {
		return "", Key{}, err
	}

	return fmt.Sprintf("%s_%s_%s", prefix, keyID, secretStr), Key{
		ID:        keyID,
		UserID:    userID,
		Name:      name,
		Hash:      base64.RawStdEncoding.EncodeToString(hash),
		Salt:      base64.RawStdEncoding.EncodeToString(salt),
		CreatedAt: time.Now().UnixMilli(),
	}, nil
}

// derive hashes a secret with a salted HMAC-SHA256.
//
// A deliberately slow KDF (PBKDF2, argon2, bcrypt) exists to make GUESSING
// expensive, which matters when the input is a human-chosen password drawn
// from a small, skewed distribution. This secret is 256 bits straight from
// crypto/rand: there is no distribution to exploit and nothing to guess, so a
// work factor buys no security at all. Measured, PBKDF2 at 100k iterations
// added 20 ms to every authenticated request - a real latency cost for zero
// benefit. A fast salted hash still means a stolen database yields no usable
// keys, which is the property that actually matters here.
//
// If key material ever becomes user-chosen, this is the function to change.
func derive(secret string, salt []byte) ([]byte, error) {
	m := hmac.New(sha256.New, salt)
	m.Write([]byte(secret))
	return m.Sum(nil), nil
}

// Parse splits a token into its key id and secret.
//
// SplitN with a limit of three, not Split: the secret is base64url encoded and
// its alphabet includes "_", so an unlimited split would shred a perfectly
// valid token into four or five parts and reject it.
func Parse(token string) (keyID, secret string, err error) {
	parts := strings.SplitN(strings.TrimSpace(token), "_", 3)
	if len(parts) != 3 || parts[0] != prefix || parts[1] == "" || parts[2] == "" {
		return "", "", ErrMalformed
	}
	return parts[1], parts[2], nil
}

// Verify checks a secret against a stored key in constant time.
//
// A plain string comparison leaks how many leading bytes matched through its
// timing, which over enough requests recovers the key one byte at a time.
func Verify(k Key, secret string) error {
	if k.RevokedAt != 0 {
		return ErrUnauthorized
	}
	salt, err := base64.RawStdEncoding.DecodeString(k.Salt)
	if err != nil {
		return ErrUnauthorized
	}
	want, err := base64.RawStdEncoding.DecodeString(k.Hash)
	if err != nil {
		return ErrUnauthorized
	}
	got, err := derive(secret, salt)
	if err != nil {
		return ErrUnauthorized
	}
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrUnauthorized
	}
	return nil
}

// Store holds keys. The in-memory implementation is enough for a single-user
// local deployment; the interface is what a database-backed one would satisfy.
type Store struct {
	mu   sync.RWMutex
	keys map[string]Key
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{keys: map[string]Key{}} }

// Add stores a key.
func (s *Store) Add(k Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[k.ID] = k
}

// Authenticate verifies a bearer token and returns the owning user id.
func (s *Store) Authenticate(ctx context.Context, token string) (string, error) {
	keyID, secret, err := Parse(token)
	if err != nil {
		return "", err
	}
	s.mu.RLock()
	k, ok := s.keys[keyID]
	s.mu.RUnlock()
	if !ok {
		// Derive anyway so a request for an unknown key id takes the same time
		// as one for a known id with a wrong secret. Otherwise the response
		// time reveals which key ids exist.
		_, _ = derive(secret, make([]byte, saltLen))
		return "", ErrUnauthorized
	}
	if err := Verify(k, secret); err != nil {
		return "", err
	}

	s.mu.Lock()
	k.LastUsedAt = time.Now().UnixMilli()
	s.keys[k.ID] = k
	s.mu.Unlock()
	return k.UserID, nil
}

// Revoke marks a key unusable.
func (s *Store) Revoke(keyID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[keyID]
	if !ok {
		return false
	}
	k.RevokedAt = time.Now().UnixMilli()
	s.keys[keyID] = k
	return true
}

// Len returns the number of stored keys.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys)
}

// --------------------------------------------------------------- tickets

// TicketStore issues short-lived single-use tickets for WebSocket handshakes.
//
// A browser cannot set an Authorization header on a WebSocket upgrade, and
// putting the API key in the query string writes it into access logs, proxy
// logs and browser history. A ticket is fetched over an authenticated POST and
// is worthless thirty seconds later.
type TicketStore struct {
	mu      sync.Mutex
	tickets map[string]ticket
	ttl     time.Duration
}

type ticket struct {
	userID  string
	expires time.Time
}

// NewTicketStore returns a ticket store with the given lifetime.
func NewTicketStore(ttl time.Duration) *TicketStore {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &TicketStore{tickets: map[string]ticket{}, ttl: ttl}
}

// Issue creates a ticket for a user.
func (t *TicketStore) Issue(userID string) (string, time.Time, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: entropy unavailable: %w", err)
	}
	id := "rt_" + base64.RawURLEncoding.EncodeToString(b)
	exp := time.Now().Add(t.ttl)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()
	t.tickets[id] = ticket{userID: userID, expires: exp}
	return id, exp, nil
}

// Redeem consumes a ticket, returning the user id. A ticket is valid exactly
// once: a replayed handshake fails even inside the TTL.
func (t *TicketStore) Redeem(id string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()

	tk, ok := t.tickets[id]
	if !ok {
		return "", ErrUnauthorized
	}
	delete(t.tickets, id)
	if time.Now().After(tk.expires) {
		return "", ErrUnauthorized
	}
	return tk.userID, nil
}

// sweepLocked drops expired tickets. Called on every operation, which is
// enough for a store that holds at most a handful of entries.
func (t *TicketStore) sweepLocked() {
	now := time.Now()
	for id, tk := range t.tickets {
		if now.After(tk.expires) {
			delete(t.tickets, id)
		}
	}
}

// FromToken rebuilds a stored record for an existing token.
//
// This is how a key supplied through the environment survives a restart: the
// server never stored the secret, so on boot it re-derives the hash from the
// token the operator supplied and keeps only that.
func FromToken(userID, name, token string) (Key, error) {
	keyID, secret, err := Parse(token)
	if err != nil {
		return Key{}, err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return Key{}, fmt.Errorf("auth: entropy unavailable: %w", err)
	}
	hash, err := derive(secret, salt)
	if err != nil {
		return Key{}, err
	}
	return Key{
		ID:        keyID,
		UserID:    userID,
		Name:      name,
		Hash:      base64.RawStdEncoding.EncodeToString(hash),
		Salt:      base64.RawStdEncoding.EncodeToString(salt),
		CreatedAt: time.Now().UnixMilli(),
	}, nil
}

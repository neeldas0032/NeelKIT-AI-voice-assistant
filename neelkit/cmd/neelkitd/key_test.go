package main

import (
	"context"
	"testing"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/auth"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/storage"
)

// The key printed on first run must still work after a restart. Before this,
// every restart minted a new in-memory key and the one saved in the browser
// started failing with 401.
func TestAPIKeySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	boot := func(rotate bool) (*auth.Store, string) {
		db, err := storage.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		uid, err := db.EnsureUser(ctx, "local")
		if err != nil {
			t.Fatal(err)
		}
		keys := auth.NewStore()
		token, err := loadOrCreateKey(ctx, db, keys, uid, rotate)
		if err != nil {
			t.Fatal(err)
		}
		return keys, token
	}

	_, first := boot(false)
	if first == "" {
		t.Fatal("first boot did not create a key")
	}

	keys, again := boot(false)
	if again != "" {
		t.Fatal("second boot created a new key instead of loading the stored one")
	}
	if _, err := keys.Authenticate(ctx, first); err != nil {
		t.Fatalf("first-run key rejected after restart: %v", err)
	}

	keys, rotated := boot(true)
	if rotated == "" {
		t.Fatal("-rotate-key did not issue a new key")
	}
	if _, err := keys.Authenticate(ctx, first); err == nil {
		t.Fatal("old key still accepted after rotation")
	}
	if _, err := keys.Authenticate(ctx, rotated); err != nil {
		t.Fatalf("rotated key rejected: %v", err)
	}

	keys, _ = boot(false)
	if _, err := keys.Authenticate(ctx, first); err == nil {
		t.Fatal("revoked key accepted again after a later restart")
	}
	if _, err := keys.Authenticate(ctx, rotated); err != nil {
		t.Fatalf("rotated key rejected after restart: %v", err)
	}
}

package auth

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestGenerateAndVerify(t *testing.T) {
	token, k, err := Generate("usr_1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "nk_") {
		t.Fatalf("token %q has no recognisable prefix", token)
	}
	// The secret must not be recoverable from what is stored.
	_, secret, err := Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(k.Hash, secret) || k.Hash == secret {
		t.Fatal("the stored hash contains the secret")
	}
	if err := Verify(k, secret); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := Verify(k, secret+"x"); err == nil {
		t.Fatal("a wrong secret verified")
	}
}

func TestGenerateIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		token, _, err := Generate("u", "n")
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] {
			t.Fatal("Generate produced a duplicate token")
		}
		seen[token] = true
	}
}

func TestParseRejectsMalformedTokens(t *testing.T) {
	for _, bad := range []string{"", "nope", "nk_", "nk_abc", "xx_a_b", "nk__b", "nk_a_", "a_b_c_d"} {
		if _, _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded", bad)
		}
	}
}

func TestRevokedKeyFails(t *testing.T) {
	token, k, _ := Generate("usr_1", "n")
	_, secret, _ := Parse(token)
	k.RevokedAt = time.Now().UnixMilli()
	if err := Verify(k, secret); err == nil {
		t.Fatal("a revoked key verified")
	}
}

func TestStoreAuthenticate(t *testing.T) {
	s := NewStore()
	token, k, _ := Generate("usr_1", "n")
	s.Add(k)

	uid, err := s.Authenticate(context.Background(), token)
	if err != nil || uid != "usr_1" {
		t.Fatalf("got %q, %v", uid, err)
	}
	if _, err := s.Authenticate(context.Background(), "nk_deadbeef_wrongsecret"); err == nil {
		t.Fatal("unknown key authenticated")
	}
	if !s.Revoke(k.ID) {
		t.Fatal("Revoke reported false")
	}
	if _, err := s.Authenticate(context.Background(), token); err == nil {
		t.Fatal("revoked key still authenticates")
	}
}

// A browser cannot set headers on a WebSocket handshake, so the ticket is the
// credential. It must be worthless the moment it is used.
func TestTicketIsSingleUse(t *testing.T) {
	ts := NewTicketStore(time.Minute)
	id, exp, err := ts.Issue("usr_1")
	if err != nil {
		t.Fatal(err)
	}
	if !exp.After(time.Now()) {
		t.Fatal("ticket issued already expired")
	}
	uid, err := ts.Redeem(id)
	if err != nil || uid != "usr_1" {
		t.Fatalf("first redeem: %q, %v", uid, err)
	}
	if _, err := ts.Redeem(id); err == nil {
		t.Fatal("a ticket was redeemed twice")
	}
}

func TestTicketExpires(t *testing.T) {
	ts := NewTicketStore(10 * time.Millisecond)
	id, _, _ := ts.Issue("usr_1")
	time.Sleep(30 * time.Millisecond)
	if _, err := ts.Redeem(id); err == nil {
		t.Fatal("an expired ticket was accepted")
	}
}

func TestUnknownTicketRejected(t *testing.T) {
	ts := NewTicketStore(time.Minute)
	if _, err := ts.Redeem("rt_not-a-real-ticket"); err == nil {
		t.Fatal("an unknown ticket was accepted")
	}
}

func BenchmarkAuthenticate(b *testing.B) {
	// PBKDF2 is intentionally slow; this measures how slow, because the number
	// bounds request throughput on every authenticated call.
	s := NewStore()
	token, k, _ := Generate("usr_1", "n")
	s.Add(k)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Authenticate(ctx, token)
	}
}

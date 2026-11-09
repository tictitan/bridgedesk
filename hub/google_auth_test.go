package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"testing"
	"time"
)

// mintToken builds a signed RS256 JWT and registers its key in the JWKS cache, so we can test
// the verifier end-to-end without talking to Google.
func mintToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	head := enc(map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"})
	body := enc(claims)
	signing := head + "." + body
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestVerifyGoogleIDToken(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-key-1"
	// Inject our key into the cache and keep it fresh so refresh() (network) is never called.
	googleKeys.mu.Lock()
	googleKeys.keys = map[string]*rsa.PublicKey{kid: &key.PublicKey}
	googleKeys.exp = time.Now().Add(time.Hour)
	googleKeys.mu.Unlock()

	base := func() map[string]any {
		return map[string]any{
			"iss": "https://accounts.google.com", "aud": "client-123.apps.googleusercontent.com",
			"email": "me@gmail.com", "email_verified": true, "exp": time.Now().Add(time.Hour).Unix(),
		}
	}

	// valid
	if c, err := verifyGoogleIDToken(mintToken(t, key, kid, base())); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	} else if c.Email != "me@gmail.com" {
		t.Fatalf("wrong email %q", c.Email)
	}

	// expired
	ex := base()
	ex["exp"] = time.Now().Add(-time.Minute).Unix()
	if _, err := verifyGoogleIDToken(mintToken(t, key, kid, ex)); err == nil {
		t.Error("expired token should be rejected")
	}

	// unverified email
	uv := base()
	uv["email_verified"] = false
	if _, err := verifyGoogleIDToken(mintToken(t, key, kid, uv)); err == nil {
		t.Error("unverified email should be rejected")
	}

	// wrong issuer
	wi := base()
	wi["iss"] = "https://evil.example"
	if _, err := verifyGoogleIDToken(mintToken(t, key, kid, wi)); err == nil {
		t.Error("bad issuer should be rejected")
	}

	// tampered signature (sign with a different key)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, err := verifyGoogleIDToken(mintToken(t, other, kid, base())); err == nil {
		t.Error("token signed by an unknown key should be rejected")
	}

	// unknown kid, cache fresh -> no key found
	if _, err := verifyGoogleIDToken(mintToken(t, key, "nope", base())); err == nil {
		t.Error("unknown kid should be rejected")
	}
}

func TestGoogleGateAllowlist(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "k"
	googleKeys.mu.Lock()
	googleKeys.keys = map[string]*rsa.PublicKey{kid: &key.PublicKey}
	googleKeys.exp = time.Now().Add(time.Hour)
	googleKeys.mu.Unlock()

	mk := func(email, aud string) string {
		return mintToken(t, key, kid, map[string]any{
			"iss": "accounts.google.com", "aud": aud, "email": email,
			"email_verified": true, "exp": time.Now().Add(time.Hour).Unix(),
		})
	}

	s := &Server{cfg: &Config{GoogleEmails: []string{"Me@Gmail.com"}}}
	if _, err := s.googleGate(mk("me@gmail.com", "any")); err != nil {
		t.Errorf("allowlisted email (case-insensitive) should pass: %v", err)
	}
	if _, err := s.googleGate(mk("intruder@gmail.com", "any")); err == nil {
		t.Error("non-allowlisted email should fail")
	}

	// with a client-ID allowlist, audience must match
	s.cfg.GoogleClients = []string{"trusted.apps.googleusercontent.com"}
	if _, err := s.googleGate(mk("me@gmail.com", "trusted.apps.googleusercontent.com")); err != nil {
		t.Errorf("matching aud should pass: %v", err)
	}
	if _, err := s.googleGate(mk("me@gmail.com", "someone-else")); err == nil {
		t.Error("non-matching aud should fail when a client allowlist is set")
	}
}

func TestLoopbackExemptFromGoogleGate(t *testing.T) {
	// The gate must apply to LAN requests but never to the owner's own loopback path.
	mk := func(remote string) *http.Request {
		r, _ := http.NewRequest("GET", "/api/devices", nil)
		r.RemoteAddr = remote
		return r
	}
	if isLoopbackReq(mk("203.0.113.9:5000")) {
		t.Error("public IP should not be loopback")
	}
	for _, lo := range []string{"127.0.0.1:5000", "[::1]:5000"} {
		if !isLoopbackReq(mk(lo)) {
			t.Errorf("%s should be loopback", lo)
		}
	}
}

var _ = big.NewInt

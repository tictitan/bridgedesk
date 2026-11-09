package main

// google_auth.go — verifies Google OpenID Connect ID tokens so the hub can require that a
// connecting client (phone, dashboard) is signed in to one of the user's own Google accounts.
//
// The client signs in with Google (loopback + PKCE, the same flow the Aria desktop app uses)
// and sends the resulting id_token to the hub. The hub verifies it fully offline-of-Google-login:
// it fetches Google's public signing keys (JWKS, cached), checks the RS256 signature, the issuer,
// expiry, that the email is verified, and that the email is on the configured allowlist. An
// optional client-ID allowlist pins the token's audience; when empty, any Google client is
// accepted (so other people can "bring their own OAuth client") and the email remains the gate.
//
// Verification uses only the Go standard library — no new dependencies, so the build stays offline.

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const googleJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"

var googleIssuers = map[string]bool{
	"accounts.google.com":         true,
	"https://accounts.google.com": true,
}

// ---- JWKS cache ----

type jwksCache struct {
	mu   sync.RWMutex
	keys map[string]*rsa.PublicKey // kid -> key
	exp  time.Time
}

var googleKeys = &jwksCache{keys: map[string]*rsa.PublicKey{}}

func (c *jwksCache) get(kid string) *rsa.PublicKey {
	c.mu.RLock()
	k, ok := c.keys[kid]
	fresh := time.Now().Before(c.exp)
	c.mu.RUnlock()
	if ok && fresh {
		return k
	}
	c.refresh()
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.keys[kid]
}

func (c *jwksCache) refresh() {
	req, _ := http.NewRequest("GET", googleJWKSURL, nil)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
			Kty string `json:"kty"`
		} `json:"keys"`
	}
	if json.NewDecoder(resp.Body).Decode(&doc) != nil {
		return
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nb, err1 := base64.RawURLEncoding.DecodeString(k.N)
		eb, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		e := 0
		for _, b := range eb {
			e = e<<8 | int(b)
		}
		if e == 0 {
			e = int(binary.BigEndian.Uint32(append(make([]byte, 4-len(eb)), eb...)))
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}
	}
	if len(keys) == 0 {
		return
	}
	// honour Cache-Control max-age when present, else default 1h
	ttl := time.Hour
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		if i := strings.Index(cc, "max-age="); i >= 0 {
			var secs int
			fmt.Sscanf(cc[i+len("max-age="):], "%d", &secs)
			if secs > 0 {
				ttl = time.Duration(secs) * time.Second
			}
		}
	}
	c.mu.Lock()
	c.keys = keys
	c.exp = time.Now().Add(ttl)
	c.mu.Unlock()
}

// googleClaims is the subset of the ID token we care about.
type googleClaims struct {
	Iss           string `json:"iss"`
	Aud           string `json:"aud"`
	Exp           int64  `json:"exp"`
	Email         string `json:"email"`
	EmailVerified any    `json:"email_verified"` // Google sends bool; be liberal
	HD            string `json:"hd"`
}

// verifyGoogleIDToken checks the signature and standard claims and returns the verified claims.
// It does NOT apply the hub's email/client allowlists — the caller does that against config.
func verifyGoogleIDToken(token string) (*googleClaims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("not a JWT")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &hdr) != nil {
		return nil, fmt.Errorf("bad header")
	}
	if hdr.Alg != "RS256" {
		return nil, fmt.Errorf("unexpected alg %q", hdr.Alg)
	}
	key := googleKeys.get(hdr.Kid)
	if key == nil {
		return nil, fmt.Errorf("unknown signing key")
	}
	if err := verifyRS256(parts[0]+"."+parts[1], parts[2], key); err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("bad payload")
	}
	var c googleClaims
	if json.Unmarshal(pb, &c) != nil {
		return nil, fmt.Errorf("bad claims")
	}
	if !googleIssuers[c.Iss] {
		return nil, fmt.Errorf("bad issuer %q", c.Iss)
	}
	if c.Exp == 0 || time.Now().After(time.Unix(c.Exp, 0)) {
		return nil, fmt.Errorf("token expired")
	}
	if !truthy(c.EmailVerified) {
		return nil, fmt.Errorf("email not verified")
	}
	if c.Email == "" {
		return nil, fmt.Errorf("no email claim")
	}
	return &c, nil
}

// googleGate verifies the request's Google token against the hub's allowlists.
// Returns the verified email and whether the request may proceed.
func (s *Server) googleGate(token string) (string, error) {
	c, err := verifyGoogleIDToken(token)
	if err != nil {
		return "", err
	}
	email := strings.ToLower(strings.TrimSpace(c.Email))
	allowed := false
	for _, e := range s.cfg.GoogleEmails {
		if strings.ToLower(strings.TrimSpace(e)) == email {
			allowed = true
			break
		}
	}
	if !allowed {
		return email, fmt.Errorf("account %s is not allowed", email)
	}
	if len(s.cfg.GoogleClients) > 0 {
		ok := false
		for _, id := range s.cfg.GoogleClients {
			if strings.TrimSpace(id) == c.Aud {
				ok = true
				break
			}
		}
		if !ok {
			return email, fmt.Errorf("token audience not accepted")
		}
	}
	return email, nil
}

// googleToken pulls the ID token from the request: header, query, or the cookie the hub sets
// after a verified load (so the dashboard WebView's later same-origin requests carry it).
func googleToken(r *http.Request) string {
	if h := r.Header.Get("X-Aria-Google"); h != "" {
		return h
	}
	if q := r.URL.Query().Get("gid"); q != "" {
		return q
	}
	if ck, err := r.Cookie("aria_gid"); err == nil {
		return ck.Value
	}
	return ""
}

func verifyRS256(signingInput, sigB64 string, key *rsa.PublicKey) error {
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("bad signature encoding")
	}
	sum := sha256.Sum256([]byte(signingInput))
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig)
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1"
	case float64:
		return x != 0
	}
	return false
}

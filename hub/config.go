package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	defaultLANPort   = 47810 // HTTPS + WSS for phones / other PCs
	defaultLocalPort = 47811 // plain HTTP, bound to 127.0.0.1 only (dashboard + MCP)
	discoveryPort    = 47812 // UDP LAN discovery
	appVersion       = "1.0.0"
)

// LinkTarget is a remote hub this machine registers itself with (e.g. a cloud relay or another PC).
type LinkTarget struct {
	URL         string `json:"url"`         // https://host:47810
	Key         string `json:"key"`         // access key of the remote hub
	Fingerprint string `json:"fingerprint"` // SHA-256 of the remote hub certificate (pinned)
}

type Config struct {
	Name        string       `json:"name"`
	AccessKey   string       `json:"accessKey"`
	LANPort     int          `json:"lanPort"`
	LocalPort   int          `json:"localPort"`
	ListenLAN   bool         `json:"listenLan"`
	Discovery   bool         `json:"discovery"`
	AllowShell  bool         `json:"allowShell"`
	AllowScreen bool         `json:"allowScreen"`
	AllowFiles  bool         `json:"allowFiles"`
	AllowMCP    bool         `json:"allowMcp"`
	MirrorNotif bool         `json:"mirrorNotif"` // raise a desktop toast when a linked phone notification arrives
	InboxDir    string       `json:"inboxDir"`    // where files shared from phones land
	Links       []LinkTarget `json:"links"`       // other hubs THIS machine dials out to (we become a device there)
	Peers       []LinkTarget `json:"peers"`       // other PCs this console controls (they dial back to us)

	mu   sync.Mutex
	path string
}

func configDir() string {
	if d := os.Getenv("BRIDGEDESK_HOME"); d != "" {
		return d
	}
	if runtime.GOOS == "windows" {
		if a := os.Getenv("APPDATA"); a != "" {
			return filepath.Join(a, "BridgeDesk")
		}
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".bridgedesk")
}

func randomKey(n int) string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/I confusion
	b := make([]byte, n)
	rand.Read(b)
	out := make([]byte, n)
	for i := range b {
		out[i] = alphabet[int(b[i])%len(alphabet)]
	}
	// group as XXXX-XXXX-XXXX-XXXX for readability
	var parts []string
	for i := 0; i < n; i += 4 {
		e := i + 4
		if e > n {
			e = n
		}
		parts = append(parts, string(out[i:e]))
	}
	return strings.Join(parts, "-")
}

func loadConfig() (*Config, error) {
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "config.json")
	c := &Config{path: p}
	data, err := os.ReadFile(p)
	if err == nil {
		if err := json.Unmarshal(data, c); err != nil {
			return nil, err
		}
	} else {
		host, _ := os.Hostname()
		home, _ := os.UserHomeDir()
		c.Name = host
		c.AccessKey = randomKey(16)
		c.LANPort = defaultLANPort
		c.LocalPort = defaultLocalPort
		c.ListenLAN = true
		c.Discovery = true
		c.AllowShell = true
		c.AllowScreen = true
		c.AllowFiles = true
		c.AllowMCP = true
		c.MirrorNotif = true
		c.InboxDir = filepath.Join(home, "Downloads", "BridgeDesk")
	}
	if c.LANPort == 0 {
		c.LANPort = defaultLANPort
	}
	if c.LocalPort == 0 {
		c.LocalPort = defaultLocalPort
	}
	if c.AccessKey == "" {
		c.AccessKey = randomKey(16)
	}
	return c, c.Save()
}

func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, _ := json.MarshalIndent(c, "", "  ")
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// loadOrCreateCert returns a long-lived self-signed certificate. Clients pin its SHA-256 fingerprint.
func loadOrCreateCert() (tls.Certificate, string, error) {
	dir := configDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if _, err := os.Stat(certPath); err != nil {
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return tls.Certificate{}, "", err
		}
		serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
		host, _ := os.Hostname()
		tmpl := x509.Certificate{
			SerialNumber:          serial,
			Subject:               pkix.Name{CommonName: "BridgeDesk " + host, Organization: []string{"BridgeDesk"}},
			NotBefore:             time.Now().Add(-24 * time.Hour),
			NotAfter:              time.Now().AddDate(20, 0, 0),
			KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			BasicConstraintsValid: true,
			IsCA:                  true,
			DNSNames:              []string{"localhost", host},
		}
		der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
		if err != nil {
			return tls.Certificate{}, "", err
		}
		kb, _ := x509.MarshalECPrivateKey(priv)
		os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
		os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return cert, "", err
	}
	return cert, fingerprint(cert.Certificate[0]), nil
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// prettyFP formats a fingerprint as groups so a human can compare the first few blocks.
func prettyFP(fp string) string {
	var parts []string
	for i := 0; i+4 <= len(fp) && i < 32; i += 4 {
		parts = append(parts, fp[i:i+4])
	}
	return strings.Join(parts, " ")
}

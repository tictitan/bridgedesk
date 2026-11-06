package main

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// runDiscovery answers LAN broadcast probes so phones/PCs find this hub without typing an IP.
func (s *Server) runDiscovery() {
	addr := &net.UDPAddr{IP: net.IPv4zero, Port: discoveryPort}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		log.Printf("discovery disabled: %v", err)
		return
	}
	defer conn.Close()
	buf := make([]byte, 1024)
	info := s.hub.local.Info()
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if string(buf[:n]) != "BRIDGEDESK?" {
			continue
		}
		reply, _ := json.Marshal(map[string]any{
			"service":     "bridgedesk",
			"name":        info.Name,
			"kind":        info.Kind,
			"os":          info.OS,
			"port":        s.cfg.LANPort,
			"fingerprint": s.fp,
			"version":     appVersion,
		})
		conn.WriteToUDP(reply, src)
	}
}

// dialPeer keeps a mesh connection to another hub alive, reconnecting as needed.
func (s *Server) dialPeer(link LinkTarget, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		isSelf := s.dialPeerOnce(link)
		if isSelf {
			s.bookMu.Lock()
			s.selfURLs[link.URL] = true
			delete(s.dialStops, link.URL)
			s.bookMu.Unlock()
			return // this URL is us; stop dialing it
		}
		select {
		case <-stop:
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// dialPeerOnce performs one mesh connection attempt. Returns true if the far end is actually us.
func (s *Server) dialPeerOnce(link LinkTarget) (isSelf bool) {
	wsURL := "wss" + link.URL[len("https"):] + "/link?key=" + link.Key
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				return verifyPinned(raw, link.Fingerprint)
			},
		},
	}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return false
	}
	// handshake: announce ourselves as a peer hub, then read the far hub's identity
	conn.WriteJSON(map[string]any{"hello": s.hub.local.Info(), "peer": true})
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var reply struct {
		Hello DeviceInfo `json:"hello"`
	}
	if err := conn.ReadJSON(&reply); err != nil || reply.Hello.ID == "" {
		conn.Close()
		return false
	}
	if reply.Hello.ID == s.hub.local.Info().ID {
		conn.Close()
		return true
	}
	s.bookMu.Lock()
	s.dialConns[link.URL] = conn
	s.bookMu.Unlock()
	s.runPeerSession(conn, reply.Hello, true)
	s.bookMu.Lock()
	delete(s.dialConns, link.URL)
	s.bookMu.Unlock()
	return false
}

// pinnedClient builds an HTTP client that trusts only the cert matching fp (self-signed peers).
func pinnedClient(fp string) *http.Client {
	return &http.Client{
		Timeout: 12 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
				VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
					return verifyPinned(raw, fp)
				},
			},
		},
	}
}

// instructPeer tells another hub (authenticated with its key, pinned to its cert) to dial back to us,
// so that peer shows up as a controllable device in THIS console's device list.
func (s *Server) instructPeer(peerHost string, peerPort int, peerKey, peerFp string, myHost string) error {
	body, _ := json.Marshal(map[string]any{
		"host": myHost, "port": s.cfg.LANPort, "key": s.cfg.AccessKey, "fingerprint": s.fp, "dial": true,
	})
	url := fmt.Sprintf("https://%s:%d/api/links", peerHost, peerPort)
	req, _ := http.NewRequest("POST", url, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+peerKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := pinnedClient(peerFp).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("peer refused (status %d) — check its access key", resp.StatusCode)
	}
	var r struct{ Error string }
	json.NewDecoder(resp.Body).Decode(&r)
	if r.Error != "" {
		return errors.New(r.Error)
	}
	return nil
}

// unlinkPeer tells a peer to stop dialing back to us.
func (s *Server) unlinkPeer(peerURL, peerKey, peerFp, myHost string) {
	u, err := urlParse(peerURL)
	if err != nil {
		return
	}
	mine := fmt.Sprintf("https://%s:%d", myHost, s.cfg.LANPort)
	req, _ := http.NewRequest("DELETE", u+"/api/links?url="+mine, nil)
	req.Header.Set("Authorization", "Bearer "+peerKey)
	resp, err := pinnedClient(peerFp).Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func urlParse(s string) (string, error) {
	if !strings.HasPrefix(s, "http") {
		return "", errors.New("bad url")
	}
	return s, nil
}

// fetchFingerprint dials the target hub over TLS and returns its certificate SHA-256,
// so the user only needs to type the other PC's address + key (we pin the cert automatically).
func fetchFingerprint(host string, port int) (string, error) {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 8 * time.Second}, "tcp",
		net.JoinHostPort(host, strconv.Itoa(port)), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("no certificate presented")
	}
	sum := sha256.Sum256(certs[0].Raw)
	return strings.ToUpper(hex.EncodeToString(sum[:])), nil
}

func verifyPinned(raw [][]byte, want string) error {
	if want == "" {
		return nil
	}
	for _, c := range raw {
		sum := sha256.Sum256(c)
		if strings.EqualFold(hex.EncodeToString(sum[:]), strings.ReplaceAll(want, " ", "")) {
			return nil
		}
	}
	return errors.New("remote hub certificate fingerprint mismatch")
}

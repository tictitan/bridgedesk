package main

// email.go — multi-account mail for Aria (phone), exposed to the relay's Claude CLI as the
// email_* MCP tools. Each account is a Gmail or Outlook/Microsoft-365 mailbox the user enrolled
// on the PC (loopback OAuth); the hub keeps only a refresh token and mints short-lived access
// tokens on demand. Reading (list/read/search) is a free tool; sending is a change tool gated by
// the phone's "Allow changes" just like the fleet tools.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type EmailAccount struct {
	Provider     string `json:"provider"` // "gmail" | "outlook"
	Email        string `json:"email"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret,omitempty"`
	RefreshToken string `json:"refreshToken"`
}

const (
	googleTokenURL = "https://oauth2.googleapis.com/token"
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	gmailAPI       = "https://gmail.googleapis.com/gmail/v1/users/me"
	msTokenURL     = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
	msAuthURL      = "https://login.microsoftonline.com/common/oauth2/v2.0/authorize"
	graphAPI       = "https://graph.microsoft.com/v1.0/me"

	gmailScopes = "openid email https://www.googleapis.com/auth/gmail.modify https://www.googleapis.com/auth/gmail.send"
	msScopes    = "offline_access openid email User.Read Mail.Read Mail.Send"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// ---- account lookup ----

func (s *Server) emailAccount(email string) (*EmailAccount, error) {
	accs := s.cfg.EmailAccounts
	if email == "" {
		if len(accs) == 1 {
			return &accs[0], nil
		}
		if len(accs) == 0 {
			return nil, fmt.Errorf("no email accounts set up (run: bridgedesk email add-gmail / add-outlook on the PC)")
		}
		return nil, fmt.Errorf("several accounts configured; pass account=<email>")
	}
	for i := range accs {
		if strings.EqualFold(accs[i].Email, email) {
			return &accs[i], nil
		}
	}
	return nil, fmt.Errorf("no such account %q", email)
}

func (s *Server) emailAccountList() []map[string]string {
	out := []map[string]string{}
	for _, a := range s.cfg.EmailAccounts {
		out = append(out, map[string]string{"email": a.Email, "provider": a.Provider})
	}
	return out
}

// ---- OAuth: refresh -> access token ----

func (a *EmailAccount) accessToken() (string, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {a.RefreshToken}, "client_id": {a.ClientID}}
	tokURL := googleTokenURL
	if a.Provider == "outlook" {
		tokURL = msTokenURL
		form.Set("scope", msScopes)
	}
	if a.ClientSecret != "" {
		form.Set("client_secret", a.ClientSecret)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := postForm(tokURL, form, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("token refresh failed: %s", firstNonEmpty(out.ErrorDesc, out.Error, "no access_token"))
	}
	return out.AccessToken, nil
}

// ---- public ops (dispatched from the MCP tools) ----

func (s *Server) emailList(accEmail, query string, limit int) (any, error) {
	a, err := s.emailAccount(accEmail)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 15
	}
	tok, err := a.accessToken()
	if err != nil {
		return nil, err
	}
	if a.Provider == "outlook" {
		return msList(tok, query, limit)
	}
	return gmailList(tok, query, limit)
}

func (s *Server) emailRead(accEmail, id string) (any, error) {
	a, err := s.emailAccount(accEmail)
	if err != nil {
		return nil, err
	}
	tok, err := a.accessToken()
	if err != nil {
		return nil, err
	}
	if a.Provider == "outlook" {
		return msRead(tok, id)
	}
	return gmailRead(tok, id)
}

func (s *Server) emailSend(accEmail, to, subject, body, cc string) (any, error) {
	a, err := s.emailAccount(accEmail)
	if err != nil {
		return nil, err
	}
	tok, err := a.accessToken()
	if err != nil {
		return nil, err
	}
	if a.Provider == "outlook" {
		return msSend(tok, to, cc, subject, body)
	}
	return gmailSend(tok, a.Email, to, cc, subject, body)
}

// ===================== Gmail =====================

func gmailList(tok, query string, limit int) (any, error) {
	q := url.Values{"maxResults": {fmt.Sprint(limit)}}
	if query != "" {
		q.Set("q", query)
	}
	var ids struct {
		Messages []struct{ ID string `json:"id"` } `json:"messages"`
	}
	if err := apiGET(gmailAPI+"/messages?"+q.Encode(), tok, &ids); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, m := range ids.Messages {
		var full struct {
			Snippet  string   `json:"snippet"`
			LabelIds []string `json:"labelIds"`
			Payload  struct {
				Headers []struct{ Name, Value string } `json:"headers"`
			} `json:"payload"`
		}
		if err := apiGET(gmailAPI+"/messages/"+m.ID+"?format=metadata&metadataHeaders=From&metadataHeaders=Subject&metadataHeaders=Date", tok, &full); err != nil {
			continue
		}
		h := func(n string) string {
			for _, x := range full.Payload.Headers {
				if strings.EqualFold(x.Name, n) {
					return x.Value
				}
			}
			return ""
		}
		out = append(out, map[string]any{
			"id": m.ID, "from": h("From"), "subject": h("Subject"), "date": h("Date"),
			"snippet": full.Snippet, "unread": contains(full.LabelIds, "UNREAD"),
		})
	}
	return out, nil
}

func gmailRead(tok, id string) (any, error) {
	var msg struct {
		Snippet string `json:"snippet"`
		Payload mimePart `json:"payload"`
	}
	if err := apiGET(gmailAPI+"/messages/"+id+"?format=full", tok, &msg); err != nil {
		return nil, err
	}
	h := func(n string) string {
		for _, x := range msg.Payload.Headers {
			if strings.EqualFold(x.Name, n) {
				return x.Value
			}
		}
		return ""
	}
	return map[string]any{
		"id": id, "from": h("From"), "to": h("To"), "subject": h("Subject"), "date": h("Date"),
		"body": gmailBody(&msg.Payload),
	}, nil
}

type mimePart struct {
	MimeType string                         `json:"mimeType"`
	Headers  []struct{ Name, Value string } `json:"headers"`
	Body     struct {
		Data string `json:"data"`
	} `json:"body"`
	Parts []mimePart `json:"parts"`
}

// gmailBody walks the MIME tree for the best text: prefer text/plain, fall back to stripped HTML.
func gmailBody(p *mimePart) string {
	if plain := findPart(p, "text/plain"); plain != "" {
		return plain
	}
	if html := findPart(p, "text/html"); html != "" {
		return stripHTML(html)
	}
	return ""
}

func findPart(p *mimePart, mime string) string {
	if strings.HasPrefix(p.MimeType, mime) && p.Body.Data != "" {
		if b, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(p.Body.Data); err == nil {
			return string(b)
		}
	}
	for i := range p.Parts {
		if r := findPart(&p.Parts[i], mime); r != "" {
			return r
		}
	}
	return ""
}

func gmailSend(tok, from, to, cc, subject, body string) (any, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\n", from, to)
	if cc != "" {
		fmt.Fprintf(&b, "Cc: %s\r\n", cc)
	}
	fmt.Fprintf(&b, "Subject: %s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s", subject, body)
	raw := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b.Bytes())
	payload, _ := json.Marshal(map[string]string{"raw": raw})
	var out struct {
		ID    string `json:"id"`
		Error struct{ Message string } `json:"error"`
	}
	if err := apiPOST(gmailAPI+"/messages/send", tok, payload, &out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, fmt.Errorf("send failed: %s", out.Error.Message)
	}
	return map[string]any{"sent": true, "id": out.ID}, nil
}

// ===================== Outlook / Microsoft 365 =====================

func msList(tok, query string, limit int) (any, error) {
	q := url.Values{"$top": {fmt.Sprint(limit)}, "$select": {"id,subject,from,bodyPreview,receivedDateTime,isRead"}}
	path := "/messages?"
	if query != "" {
		q.Set("$search", `"`+query+`"`)
	} else {
		q.Set("$orderby", "receivedDateTime desc")
	}
	var res struct {
		Value []struct {
			ID               string `json:"id"`
			Subject          string `json:"subject"`
			BodyPreview      string `json:"bodyPreview"`
			ReceivedDateTime string `json:"receivedDateTime"`
			IsRead           bool   `json:"isRead"`
			From             struct {
				EmailAddress struct{ Address, Name string } `json:"emailAddress"`
			} `json:"from"`
		} `json:"value"`
	}
	if err := apiGET(graphAPI+path+q.Encode(), tok, &res); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, m := range res.Value {
		from := m.From.EmailAddress.Address
		if m.From.EmailAddress.Name != "" {
			from = m.From.EmailAddress.Name + " <" + from + ">"
		}
		out = append(out, map[string]any{
			"id": m.ID, "from": from, "subject": m.Subject, "date": m.ReceivedDateTime,
			"snippet": m.BodyPreview, "unread": !m.IsRead,
		})
	}
	return out, nil
}

func msRead(tok, id string) (any, error) {
	var m struct {
		Subject          string `json:"subject"`
		ReceivedDateTime string `json:"receivedDateTime"`
		From             struct {
			EmailAddress struct{ Address, Name string } `json:"emailAddress"`
		} `json:"from"`
		ToRecipients []struct {
			EmailAddress struct{ Address string } `json:"emailAddress"`
		} `json:"toRecipients"`
		Body struct {
			ContentType string `json:"contentType"`
			Content     string `json:"content"`
		} `json:"body"`
	}
	if err := apiGET(graphAPI+"/messages/"+url.PathEscape(id)+"?$select=subject,from,toRecipients,body,receivedDateTime", tok, &m); err != nil {
		return nil, err
	}
	body := m.Body.Content
	if strings.EqualFold(m.Body.ContentType, "html") {
		body = stripHTML(body)
	}
	to := []string{}
	for _, r := range m.ToRecipients {
		to = append(to, r.EmailAddress.Address)
	}
	return map[string]any{
		"id": id, "from": m.From.EmailAddress.Address, "to": strings.Join(to, ", "),
		"subject": m.Subject, "date": m.ReceivedDateTime, "body": body,
	}, nil
}

func msSend(tok, to, cc, subject, body string) (any, error) {
	rcpt := func(csv string) []map[string]any {
		out := []map[string]any{}
		for _, s := range strings.Split(csv, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, map[string]any{"emailAddress": map[string]string{"address": s}})
			}
		}
		return out
	}
	msg := map[string]any{
		"message": map[string]any{
			"subject":      subject,
			"body":         map[string]string{"contentType": "Text", "content": body},
			"toRecipients": rcpt(to),
		},
		"saveToSentItems": true,
	}
	if cc != "" {
		msg["message"].(map[string]any)["ccRecipients"] = rcpt(cc)
	}
	payload, _ := json.Marshal(msg)
	if err := apiPOST(graphAPI+"/sendMail", tok, payload, nil); err != nil {
		return nil, err
	}
	return map[string]any{"sent": true}, nil
}

// ===================== enrollment (loopback + PKCE, run on the PC) =====================

func enrollGmail(clientID, clientSecret string) (*EmailAccount, error) {
	return enroll("gmail", clientID, clientSecret, googleAuthURL, googleTokenURL, gmailScopes,
		"&access_type=offline&prompt=consent")
}

func enrollOutlook(clientID, clientSecret string) (*EmailAccount, error) {
	return enroll("outlook", clientID, clientSecret, msAuthURL, msTokenURL, msScopes, "&prompt=select_account")
}

func enroll(provider, clientID, clientSecret, authURL, tokURL, scopes, extra string) (*EmailAccount, error) {
	if clientID == "" {
		return nil, fmt.Errorf("a client ID is required")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)

	vb := make([]byte, 48)
	rand.Read(vb)
	verifier := base64.RawURLEncoding.EncodeToString(vb)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	auth := authURL + "?client_id=" + url.QueryEscape(clientID) +
		"&redirect_uri=" + url.QueryEscape(redirect) +
		"&response_type=code&scope=" + url.QueryEscape(scopes) +
		"&code_challenge=" + challenge + "&code_challenge_method=S256" + extra

	codeCh := make(chan string, 1)
	srv := &http.Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		w.Header().Set("Content-Type", "text/html")
		if code == "" {
			fmt.Fprintf(w, "<body style='font-family:sans-serif'>Sign-in failed: %s</body>", r.URL.Query().Get("error"))
		} else {
			fmt.Fprint(w, "<body style='font-family:sans-serif'>✓ Mailbox connected to Aria. You can close this tab.</body>")
		}
		select {
		case codeCh <- code:
		default:
		}
	})
	srv.Handler = mux
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Println("Opening your browser to sign in… (if it doesn't open, paste this URL):")
	fmt.Println(auth)
	openTarget(auth)

	var code string
	select {
	case code = <-codeCh:
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("timed out waiting for sign-in")
	}
	if code == "" {
		return nil, fmt.Errorf("no authorization code returned")
	}

	form := url.Values{
		"code": {code}, "client_id": {clientID}, "code_verifier": {verifier},
		"redirect_uri": {redirect}, "grant_type": {"authorization_code"},
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	if provider == "outlook" {
		form.Set("scope", scopes)
	}
	var tok struct {
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := postForm(tokURL, form, &tok); err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh token returned: %s", firstNonEmpty(tok.ErrorDesc, tok.Error))
	}
	email := emailFromIDToken(tok.IDToken)
	if email == "" && tok.AccessToken != "" {
		email = whoami(provider, tok.AccessToken)
	}
	return &EmailAccount{Provider: provider, Email: email, ClientID: clientID, ClientSecret: clientSecret, RefreshToken: tok.RefreshToken}, nil
}

func whoami(provider, accessToken string) string {
	if provider == "outlook" {
		var me struct {
			Mail string `json:"mail"`
			UPN  string `json:"userPrincipalName"`
		}
		if apiGET(graphAPI, accessToken, &me) == nil {
			return firstNonEmpty(me.Mail, me.UPN)
		}
	}
	return ""
}

// ===================== shared HTTP helpers =====================

func apiGET(u, token string, out any) error  { return apiDo("GET", u, token, nil, out) }
func apiPOST(u, token string, body []byte, out any) error {
	return apiDo("POST", u, token, body, out)
}

func apiDo(method, u, token string, body []byte, out any) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, u, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s -> %d: %s", method, shortURL(u), resp.StatusCode, aiFirstLine(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func postForm(u string, form url.Values, out any) error {
	resp, err := httpClient.PostForm(u, form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if out != nil {
		json.Unmarshal(data, out)
	}
	return nil
}

func emailFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Email string `json:"email"`
		UPN   string `json:"preferred_username"`
	}
	json.Unmarshal(b, &c)
	return firstNonEmpty(c.Email, c.UPN)
}

func stripHTML(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	// collapse runaway whitespace
	lines := strings.Split(b.String(), "\n")
	out := []string{}
	for _, l := range lines {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func shortURL(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

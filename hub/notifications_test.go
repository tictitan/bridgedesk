package main

import "testing"

func TestExtractCode(t *testing.T) {
	cases := []struct{ title, text, want string }{
		{"Google", "G-482913 is your Google verification code.", "482913"},
		{"", "Your verification code is 731 204", "731204"},
		{"Desjardins", "Votre code de sécurité est 55821. Ne le partagez pas.", "55821"},
		{"Microsoft", "Use 9921 as Microsoft account security code", "9921"},
		{"Uber", "Your Uber code: 4821. Never share this code.", "4821"},
		{"WhatsApp", "Mom: see you at 2024 dinner?", ""},
		{"Amazon", "Your order #1234567 has shipped", ""},
		{"Okta Verify", "Did you just try to sign in?", ""},
		{"Bank", "OTP 004512 for your transaction of $20.00", "004512"},
		{"Twilio", "123456 est votre code de connexion", "123456"},
		{"Calendar", "Meeting at 1530 in room 4", ""},
	}
	for _, c := range cases {
		if got := extractCode(c.title, c.text); got != c.want {
			t.Errorf("%q / %q: got %q want %q", c.title, c.text, got, c.want)
		}
	}
}

func TestNotifLogQuery(t *testing.T) {
	nl := &NotifLog{max: 3, listeners: map[int]func(string, any){}, path: t.TempDir() + "/n.json"}
	cfg := &Config{MutedApps: []string{"Spammy"}}
	nl.add(Notification{Key: "a", App: "Messages", Text: "Your code is 123456", Code: "123456", Received: 1})
	nl.add(Notification{Key: "b", App: "Spammy", Text: "buy now", Received: 2})
	if fresh := nl.add(Notification{Key: "b", App: "Spammy", Text: "buy now", Received: 3}); fresh {
		t.Fatal("identical re-post should not be fresh")
	}
	nl.add(Notification{Key: "c", App: "WhatsApp", Title: "Mom", Text: "dinner?", Received: 4, Active: true})
	nl.add(Notification{Key: "d", App: "WhatsApp", Title: "Dad", Text: "ok", Received: 5, Active: true}) // evicts "a" (max 3)
	all := nl.Query(cfg, NotifQuery{})
	if len(all) != 3 || all[0].Key != "d" {
		t.Fatalf("unexpected %+v", all)
	}
	if got := nl.Query(cfg, NotifQuery{App: "whatsapp"}); len(got) != 2 {
		t.Fatalf("app filter: %d", len(got))
	}
	if got := nl.Query(cfg, NotifQuery{Q: "MOM"}); len(got) != 1 || got[0].Key != "c" {
		t.Fatalf("search: %+v", got)
	}
	if s := nl.Summary(cfg); s["unread"].(int) != 2 { // Spammy is muted
		t.Fatalf("unread %v", s["unread"])
	}
	nl.MarkRead("")
	if got := nl.Query(cfg, NotifQuery{Unread: true}); len(got) != 0 {
		t.Fatal("mark read failed")
	}
	nl.markInactive("c")
	nl.Clear()
	if got := nl.Query(cfg, NotifQuery{}); len(got) != 2 || got[0].Key != "d" || got[1].Key != "b" {
		t.Fatalf("clear should keep active ones (b re-posted, d), got %d", len(got))
	}
}

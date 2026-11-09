# Aria for Android

Aria on your phone. It's the existing BridgeDesk Android app with an **Aria chat tab**
added: you type a question, and your PC answers it. The phone holds no AI key and runs no
model — it relays your prompt to the **Claude CLI running on your PC** (the same engine the
Aria desktop app drives) through the BridgeDesk hub, and streams the reply back. Because the
PC does the thinking, Aria can see and drive your whole **BridgeDesk fleet** (your PCs and
this phone) while answering.

The app still does everything BridgeDesk did — the **Fleet** tab is the PC dashboard, and the
phone can still be shared to / controlled by your PCs — so nothing you had before is lost.

```
 Phone                         PC (hub + Claude CLI)
 ┌───────────────┐   wss /ws   ┌──────────────────────────────┐
 │ Aria chat tab │────────────▶│ hub  ──spawn──▶ claude -p      │
 │  (AiClient)   │  ai.ask     │                 (stream-json)  │
 │               │◀────────────│   ai.delta / ai.tool / result  │
 └───────────────┘             │        │ bridge_* MCP (:47811) │
                               │        ▼                        │
                               │   BridgeDesk fleet (PCs/phone)  │
                               └──────────────────────────────┘
```

## Architecture

- **Transport:** the chat opens its own TLS-pinned WebSocket to the hub's dashboard channel
  `wss://<pc>:47810/ws?key=…` — the same pinning and access key used for pairing. No new
  ports, no new trust.
- **Brain:** the hub's new `ai.ask` handler (`hub/ai.go`) runs
  `claude -p --output-format stream-json` on the PC, with the BridgeDesk MCP server
  (`http://127.0.0.1:47811/mcp`) wired in so Claude can call the `bridge_*` fleet tools.
- **Phone side:** `AiClient.java` sends the prompt and dispatches `ai.delta` (text),
  `ai.tool` (a fleet tool the PC is about to run) and the final result (which carries a
  `--resume` session id so the conversation continues across messages). The UI lives in
  `MainActivity` as the **Aria** tab.

### Fleet assistant

Two quick actions sit above the chat box:

- **Check fleet** — one coordinating turn: Aria lists every online device and, per device, reports
  what needs attention (notifications, pending 2FA, anything waiting). Read-only.
- **Run on fleet…** — type a task, then tap it: the hub runs **one Claude worker per online
  device** (`ai.fleet`, capped at 4 in parallel), each scoped to its own machine, and the phone
  shows a live section per device. Honours the **Allow changes** toggle — off = the workers can
  only read.

`ai.fleet` lives in `hub/ai.go` (`runFleet`): it enumerates `hub.List()`, spawns a `runAI` turn
per device with a device-scoped prompt, tags every streamed event with the device name
(`ai.fleet.start` / `ai.delta` / `ai.tool` / `ai.fleet.done`), and returns a per-device summary.
You can also pass a `devices` list to target specific machines instead of all.

### Safety model (same spirit as Aria desktop)

- **Reading is free:** list devices/files, read a file, screenshot a device, read
  notifications and 2FA codes.
- **Changes are gated:** running a command, writing/moving files, remote tap/type, sending
  SMS/WhatsApp, dialing, mounting/transferring, acting on a notification — these are only
  offered to the model when you turn on **"Allow changes"** for that message. The relay
  enforces this on the PC with `--allowedTools` / `--disallowedTools`, so even your PC's
  global Claude permissions can't silently auto-approve a change requested from the phone.
  Built-in escape tools (Bash, Read, Write, Monitor, Task, web, …) are always denied; the
  phone can only use the fleet tools.
- Aria never asks for, stores, or types passwords, PINs, or 2FA/OTP codes.

What's intentionally **not** on the phone: PC-only desktop autonomy (screen-watch, the
orchestrator, local browser automation). Email and the business tools live in the Aria
desktop app and are a possible phase 2 (see below).

## Build

Needs the Android command-line tools (build-tools + a platform) and a JDK 17+. No Gradle,
no Android Studio. Works on Linux/macOS (CI) and on Windows via Git Bash (the script now
resolves `.bat`/`.exe` tools and falls back to Python when `zip` is absent).

```bash
export ANDROID_SDK=/path/to/android/sdk      # or ANDROID_HOME
cd android
bash build.sh                                 # -> android/BridgeDesk.apk (signed)
```

The hub (which carries the relay) builds as before:

```bash
cd hub
bash build.sh                                 # -> dist/BridgeDesk.exe (+ other targets)
```

Install the APK on the phone (`adb install -r android/BridgeDesk.apk`, or copy + tap it).
It installs over any existing BridgeDesk build because the package and release key are
unchanged.

## Use

1. On the PC, start BridgeDesk and make sure you're signed in to the Claude CLI there
   (`claude` runs on your existing login — subscription, API key, or Bedrock, whatever the
   desktop Aria uses). The relay uses the PC's own Claude login; nothing is stored on the phone.
2. On the phone, open **Aria** and pair: scan the dashboard QR, paste a `bd://…` link, or
   type the PC address + access key (+ fingerprint). This is unchanged from BridgeDesk.
3. The **Aria** tab is home. Ask a question. Flip **Allow changes** on before asking Aria to
   do something that changes a device. The **Fleet** tab is still the full PC dashboard, and
   **⚙ → Phone access** still shares this phone with your PCs.

## Reach it from anywhere (DNS name)

You can connect over the internet, not just the LAN, by entering a **DNS name** in the connect
screen instead of a LAN IP. Because the phone pins the hub's certificate by SHA-256 fingerprint and
does no hostname check, a home **dynamic-DNS** name whose WAN IP changes still verifies — the pin
is the identity, not the address. The Aria chat (`wss://<host>:47810`), the Fleet dashboard and the
phone↔PC link all use that same host, so all three work with the hostname.

On your router:

1. **Port-forward** TCP **47810** → this PC's LAN IP (give the PC a static DHCP lease). If your ISP
   blocks 47810, forward **443 → 47810** and use port 443 in the app.
2. Point a **dynamic-DNS** name at your home — DuckDNS/No-IP, or your router's built-in DDNS
   (e.g. `myhome.duckdns.org`).

Then in the app enter that DNS name, the same access key, and the fingerprint (strongly recommended
over the internet — it's what makes the changing-IP pinning safe). No hub change is needed: the hub
already listens on all interfaces (`0.0.0.0:47810`) and the fingerprint pin covers the new host.
Combine with the Google-account lock above so a leaked key alone can't connect. The loopback MCP
port (47811) is never exposed.

## Config knobs

- `ARIA_CLAUDE_BIN` (PC env): path to the `claude` binary if it isn't auto-found.
- The chat's working directory on the PC is `~/.bridgedesk/aria-mobile` (keeps per-chat
  session state for `--resume`).

## Verifying the relay without a phone

The hub ships a guarded self-test that drives the real relay against a running hub's MCP:

```bash
cd hub
ARIA_AI_SELFTEST=1 go test -run Selftest -v          # single-turn + fleet; real Claude calls, skipped otherwise
```

## Restrict access to your Google account(s)

When the hub is reachable from the internet, you can require that every client is **signed in to
one of your Google accounts** — on top of the TLS pin and the access key (defense in depth). The
real gate is an **email allowlist**; the access key still applies.

How it works: the phone signs in with Google (loopback + PKCE, same as Aria desktop — no Google
Play Services), gets a signed **ID token**, and sends it on every connection. The hub verifies the
token offline (RS256 signature via Google's published keys, issuer, expiry, `email_verified`) and
checks the email against the allowlist. Pure Go stdlib, no new deps. See `hub/google_auth.go`
(+ tests in `hub/google_auth_test.go`) and `android/.../GoogleAuth.java`.

**Turn it on (PC):**

```bash
bridgedesk google on you@gmail.com            # require sign-in; allow this account (comma-separate more)
bridgedesk google status                      # show current setting
bridgedesk google off                         # back to key-only
```

(or POST `requireGoogle` / `googleEmails` / `googleClients` to `/api/settings`.)

**Sign in (phone):** Aria → ⚙ Phone access → *Google account* → paste your **OAuth client ID**
(and secret, for a Desktop-type client) → **Sign in**. Reuse the same client you set up for Aria
desktop. Anyone else you allow simply uses **their own** OAuth client and their own allowlisted
email — the hub accepts any Google client by default; set `googleClients` to pin specific ones.

Notes: ID tokens last ~1 h and the phone refreshes silently via the stored refresh token; the
dashboard WebView gets a short-lived `aria_gid` cookie after a verified load. The MCP port (47811)
stays loopback-only and is unaffected.

## Multi-account email (Gmail + Outlook)

Aria can read and help with several mailboxes from the phone — **no phone changes needed**: the
hub exposes `email_*` tools on its MCP, so the relay's Claude gets them automatically and you just
ask in chat ("any unread from the bank?", "reply to Sam that I'll call tomorrow"). Each mailbox is
a Gmail or Outlook/Microsoft-365 account you enrol **on the PC**; the hub keeps only a refresh
token and mints short-lived access tokens per call. See `hub/email.go`.

Tools: `email_accounts`, `email_list` (optional provider search — Gmail syntax like `is:unread
from:bob`, or Outlook keywords), `email_read`, `email_send`. Reading is free; **`email_send` is a
change** — gated by *Allow changes* and Aria shows you the recipient/subject/body first.

**Enrol a mailbox (on the PC):**

```bash
bridgedesk email add-gmail   <your-oauth-client-id> [client-secret]   # opens Google in your browser
bridgedesk email add-outlook <your-oauth-client-id> [client-secret]   # opens Microsoft sign-in
bridgedesk email list                                                 # show connected mailboxes
bridgedesk email remove you@gmail.com
```

- **Gmail:** reuse the same Google OAuth client as Aria desktop (it already grants
  `gmail.modify` + `gmail.send`). Enable the Gmail API for that client if you haven't.
- **Outlook/Microsoft 365:** an Azure app registration (public client) with delegated
  `Mail.Read`, `Mail.Send`, `offline_access`, and `http://127.0.0.1` as a redirect URI.

Add as many accounts as you like; when more than one is set up, tell Aria which (`account=`) or
she'll ask. End-to-end verification needs a mailbox enrolled and the new hub running; the offline
tests cover the MCP wiring and the ID-token/relay logic.

## Other possible work

A standalone on-phone engine (so the phone works when the PC is off). Out of scope for this build,
which keeps everything inside this repo (hub + APK) with no changes to the Aria desktop app.

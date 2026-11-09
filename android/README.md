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

## Config knobs

- `ARIA_CLAUDE_BIN` (PC env): path to the `claude` binary if it isn't auto-found.
- The chat's working directory on the PC is `~/.bridgedesk/aria-mobile` (keeps per-chat
  session state for `--resume`).

## Verifying the relay without a phone

The hub ships a guarded self-test that drives the real relay against a running hub's MCP:

```bash
cd hub
ARIA_AI_SELFTEST=1 go test -run AISelftest -v        # makes one real Claude call; skipped otherwise
```

## Possible phase 2

A standalone on-phone engine (so the phone works when the PC is off), and relaying to the
full Aria desktop app for email + business tools. Both are out of scope for this build, which
keeps everything inside this repo (hub + APK) with no changes to the Aria desktop app.

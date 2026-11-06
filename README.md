# BridgeDesk

A self-hosted remote-access bridge between your **Windows PC** and your **Android phone** — the AnyDesk/RustDesk idea, but yours: no accounts, no cloud relay, no subscription. One small program on the PC, one app on the phone, talking directly over your network with an access key and a pinned TLS certificate.

It gives you, in both directions:

| | From the phone → control the PC | From the PC → control the phone |
|---|---|---|
| **Shell** | PowerShell / cmd, full interactive terminal | `sh` shell on the phone |
| **Remote desktop** | see & control the PC screen (mouse, keyboard, Ctrl+Alt+Del) | see & control the phone screen (tap, swipe, type) |
| **Files** | browse, upload, download, rename, delete | same, into phone storage |
| **Network drive** | map the PC's shared folder like SMB (WebDAV) | — |
| **Clipboard** | copy/paste text both ways | both ways |
| **Notifications** | — | mirror the phone's notifications (SMS, chat, **Okta Verify** & other push) to the PC, and tap their actions remotely (e.g. **Approve** a login) |

And a bonus the commercial tools don't have: a built-in **MCP server**, so **Claude can drive any linked machine** — run commands, move files between devices, take screenshots, click and type — on your behalf.

> **SSH / RDP:** BridgeDesk gives you its own shell + remote-desktop channel over one pinned-TLS port, so you don't need to expose Windows RDP (3389) or an SSH server to your network. If you specifically want *real* RDP or SSH as well, see **"Using real RDP/SSH too"** below — BridgeDesk's terminal is the place to launch and tunnel them.

---

## Download

Grab the latest build from the **[Releases page](../../releases/latest)**:

| File | Run it on |
|---|---|
| `BridgeDesk.exe` | Windows PC (x64) |
| `BridgeDesk-arm64.exe` | Windows on ARM |
| `bridgedesk-linux` | Linux (x64) |
| `bridgedesk-macos` | Mac (Apple Silicon) |
| `BridgeDesk.apk` | Android phone |

Or build it yourself — see [Building from source](#building-from-source). Contributions, issues and ideas are welcome.

---

## What's in the box

```
dist/
  BridgeDesk.exe            Windows hub (x64)      ← run this on your PC
  BridgeDesk-arm64.exe      Windows hub (ARM64)
  BridgeDesk.apk            Android app            ← install this on your phone
  bridgedesk-linux          Linux hub (x64)
  bridgedesk-macos          macOS hub (ARM64, screen/input stubbed)
hub/                        Go source for the hub + web dashboard
android/                    Java source for the Android app + build.sh
```

Everything is built from source in this folder; nothing phones home.

---

## 1. Set up the PC (hub)

1. Copy **`BridgeDesk.exe`** anywhere and double-click it.
2. It adds a **BridgeDesk icon to your system tray** (next to the clock) and opens the **dashboard** in your browser — that's the graphical interface you manage everything from: Terminal, Files, Desktop, Alerts, Devices, Settings.
3. The app keeps running in the background. Manage it from the tray icon:
   - **double-click** → open the dashboard
   - **right-click** → Open BridgeDesk · Copy access key · Pause/Resume network sharing · Quit

Everything that controls BridgeDesk — devices, capabilities (shell/files/screen/notifications/MCP), the access key, autostart, the WebDAV drive — lives in the dashboard's **Settings** and **Devices** pages. There's no config file to hand-edit.

**Run as administrator (optional).** By default BridgeDesk runs as a normal user, so its shell and file access are limited to what your account can touch. For full access to protected folders and all processes, right-click the tray icon → **Restart as administrator** (one UAC prompt), or run `BridgeDesk.exe elevate`. The Settings page shows whether it's currently **Administrator** or **Standard user**. It does not force a UAC prompt on every launch — you elevate only when you want to.

> Note: even as administrator, Windows won't let *any* app click on the UAC "secure desktop" prompt remotely — that's an OS protection, not a BridgeDesk limit.

Windows may ask to allow it through the firewall on **private networks** — say yes, so your phone can reach it.

Command line:

```
BridgeDesk.exe status     # print access key, fingerprint, and the phone connection URLs
BridgeDesk.exe newkey     # rotate the access key
```

Config, the TLS certificate, and the key live in `%APPDATA%\BridgeDesk\`.

### Ports

| Port | Use | Exposure |
|---|---|---|
| 47810 | HTTPS + WSS for phones/other PCs | your LAN |
| 47811 | dashboard + MCP | **loopback only** (127.0.0.1) |
| 47812/udp | LAN auto-discovery | your LAN |

## 2. Set up the phone

1. Copy **`BridgeDesk.apk`** to the phone and open it. Allow "install from unknown sources" when prompted (it's self-signed; that's expected for a sideloaded app).
2. Open **BridgeDesk**. Enter the PC's **IP address**, the **access key**, and — recommended — the **certificate fingerprint** (both shown by `BridgeDesk.exe status` and on the PC dashboard's *Devices* page). You can also paste a `bd://ip:port?key=...&fp=...` link.
3. Tap **Connect**. You now have the PC's full dashboard on your phone — terminal, files, remote desktop of the PC.

### Adding a device (quickest: scan a QR)

On the PC dashboard → **Devices** → **+ Add device**. A dialog shows a **QR code**. Point the phone's camera (or Google Lens) at it and open the link — BridgeDesk launches with the address, port, key and fingerprint already filled in, and connects. No camera? The dialog also shows the address/port/key and a **Copy pairing link** button; type them on the phone or paste the `bd://…` link.

### Add more computers (PCs and Macs) — one mesh fleet

BridgeDesk forms a **mesh**: connect any two machines and the whole fleet converges. Add a device on **one** machine and it shows up on **every** machine — PCs, Macs, and phones all appear in every dashboard's device switcher, and you can control any of them from any of them.

- **Macs** run the same hub (`bridgedesk-macos`); install it like the Windows hub (shell, files, terminal work; screen/input are Windows-only for now).
- **To add a machine:** on any dashboard → **Devices** → **Add another computer** → enter that machine's **address** and **access key** (from its own dashboard) → **Connect**. The two mesh together, then **gossip** the rest of the fleet to each other, so after adding B and C to A, B and C also see each other automatically. The certificate is pinned automatically and the fleet reconnects after reboots.
- **Phones:** scan each phone's QR once (on any hub). A phone linked to one PC is visible and controllable from every PC in the fleet.
- **Remove** a machine with its **Remove** button — it drops off immediately and a tombstone keeps it from being re-advertised by the others. Re-add it anytime to bring it back. (To retire a machine fleet-wide, remove it on each console or just stop BridgeDesk on it.)

Control to a machine you're not directly connected to is **relayed** through the mesh, so it works even across several hops. Any machine can be the console.

### Letting Claude work across the fleet (e.g. Unreal Engine)

The whole point of the mesh is that **Claude can drive every machine from one MCP endpoint**. Point Claude at any one hub's `http://127.0.0.1:47811/mcp` (see §3) and `bridge_list_devices` shows the entire fleet; every other tool takes a `device` argument naming any machine in it. So Claude can, in one session:

- run a cook/build on your **Windows UE workstation** (`bridge_run device="WIN-UE" command="..."` — PowerShell, so `UnrealEditor-Cmd.exe`, `RunUAT.bat`, `Build.bat` all work),
- run Unreal's Python API or a commandlet to **generate assets/levels**,
- **move the generated files** to another machine with `bridge_transfer` (e.g. pull a cooked build onto a Mac or a test PC),
- take a `bridge_screenshot` of the editor to check state, and drive the UI with `bridge_input` if needed.

Long jobs are supported: relayed commands and builds may run up to **3 hours** per call. For cooks longer than that, launch them detached (`Start-Process`) and poll the log with follow-up `bridge_run` calls, or use the **Terminal** tab (a streaming session with no per-call limit).

### Let the PC control the phone

Tap **Phone access** and turn on:

- **Share this phone with the PC** — links the phone to the hub as a device (runs as a foreground service so it survives the screen locking).
- **Allow screen capture** — grant the Android prompt; the PC can now see the phone's screen (this keeps working while the phone is locked).
- **Allow remote touch & typing** — enables the BridgeDesk accessibility service so the PC's taps/swipes/typing reach the phone.
- **Full file access** — to browse all of internal storage.
- **Mirror notifications to PC** — forward the phone's notifications; opens Android's *Notification access* screen to approve BridgeDesk.

Now, on the PC dashboard, switch the device selector (top-left) to your phone and use the **Desktop**, **Files**, **Terminal**, **Alerts** tabs against the phone.

### Remote desktop tips

The **Desktop** tab streams the screen and sends your mouse, touch and keyboard live:

- **Multiple monitors** — pick *Screen 1 / Screen 2 / All screens*; clicks land on the right display.
- **Quality presets** — *Fast*, *Balanced*, *Sharp* or *Custom* (JPEG quality, width, FPS cap). Unchanged frames aren't re-sent, so an idle screen costs almost no bandwidth. The status line shows fps, KB/s and latency.
- **Touch (phone/tablet → PC)** — tap = click, hold = right-click, drag = drag/select, two fingers = scroll. The ⌨ button opens the phone keyboard.
- **Keyboard** — type normally (any layout, accents, AltGr); shortcuts like Ctrl+C, Alt+Tab, Win+R pass through. The **Keys** menu sends the ones your browser would swallow: Ctrl+Alt+Del, Task Manager, Win key, Alt+F4, Print Screen, media/volume keys, **Lock the PC**.
- **Clipboard** — 📋 sends your clipboard to the PC and pastes it; ⧉ copies the PC's clipboard to you; **Aa** types a block of text.
- **Screenshot** 📷, **fit / actual size**, and **full screen** (also works inside the Android app).
- The remote mouse pointer is drawn into the stream.

### Notifications & app prompts (e.g. Okta)

Once *Mirror notifications* is on, the PC dashboard's **Alerts** tab shows the phone's live notifications — texts, chat messages, SMS 2FA codes, and push prompts such as **Okta Verify** or Microsoft Authenticator. Each notification shows its action buttons, so you can tap **"Yes, it's me" / "Approve"** straight from the PC, or dismiss it — without picking up the phone. Turn on **Settings → Mirror phone notifications** (or the "toast on this PC" checkbox in Alerts) to also get a native Windows pop-up for each one.

Claude can do this too, through MCP: *"check my phone for an Okta prompt and approve it"* uses `bridge_notifications` + `bridge_notification_action`.

> **Privacy note:** notification mirroring can include one-time codes and login prompts, so it's gated behind your access key, the pinned certificate, and an on-phone toggle — and it only runs while you've enabled it. Keep it to networks you trust.

### About "control the phone while it's locked"

- **Viewing** a locked phone works — MediaProjection keeps streaming.
- **Touch/typing** past a *secure* lock screen (PIN/pattern/fingerprint) is **blocked by Android itself** — synthetic input can't cross the keyguard on a normal phone. Everything works once the phone is unlocked.
- For true pass-the-lock control you need a one-time elevated grant. Two options:
  - **Root:** if the phone is rooted, remote input works anywhere.
  - **ADB "device owner"** (no root): connect the phone to a computer once and run
    ```
    adb shell dpm set-device-owner com.bridgedesk.app/.ControlService
    ```
    (Requires no other account on the phone; this is the same mechanism MDM tools use.)

  These are deliberately manual — an app that could bypass your lock screen silently would be malware. BridgeDesk makes you opt in, on the device, in the open.

---

## 3. Let Claude control your machines (MCP)

The hub exposes a Model Context Protocol server at **`http://127.0.0.1:47811/mcp`** with these tools:

`bridge_list_devices`, `bridge_run`, `bridge_list_files`, `bridge_read_file`, `bridge_write_file`, `bridge_transfer` (copy a file *between* devices), `bridge_screenshot`, `bridge_input`, `bridge_clipboard`, `bridge_notifications` (read a phone's notifications), `bridge_notification_action` (tap/approve/dismiss one).

So you can ask Claude things like *"screenshot my phone"*, *"pull the newest file from my phone's Download folder onto my PC Desktop"*, or *"run `Get-Process` on my PC and summarize the top memory users."*

**Claude Desktop** (`claude_desktop_config.json`) — uses the bundled `mcp-remote` bridge:

```json
{
  "mcpServers": {
    "bridgedesk": {
      "command": "npx",
      "args": ["-y", "mcp-remote", "http://127.0.0.1:47811/mcp"]
    }
  }
}
```

Any MCP client that speaks HTTP can point straight at `http://127.0.0.1:47811/mcp`. MCP is **loopback-only** by default and can be turned off entirely in Settings → Capabilities.

---

## 4. Shared storage

### a) WebDAV drive (zero setup, cross-platform)
The hub shares its **Inbox** folder (`Downloads\BridgeDesk`) over WebDAV, which Windows, macOS, Linux and Android file managers mount natively.

- **Windows:** File Explorer → *Map network drive* → `https://<PC-IP>:47810/dav/` → when prompted, any username, password = your access key.
- **macOS:** Finder → *Go → Connect to Server* → same URL.
- **Android:** any file manager with WebDAV (e.g. CX, Solid Explorer).

Files phones send also land in this Inbox.

### b) Real SMB / NFS shares (Settings → Shared storage)
On a Windows hub you can **expose any folder as an SMB share** and **mount** SMB/NFS shares on any device, from the dashboard:

- **Share a folder** → gives you a `\\PC\share` path to mount elsewhere. (Creating a share needs admin — use the tray's *Restart as administrator*.)
- **Mount a share** → enter `\\host\share` (SMB) or `host:/export` (NFS) and a target (`Z:` on Windows, `/mnt/x` on Linux/macOS).

Claude can do this across the fleet too: `bridge_share_create` on one machine + `bridge_share_mount` on another gives the whole fleet a common drive (shared UE assets, build outputs, etc.). Linux/macOS hubs can *mount* shares; to *serve* SMB from them, use the WebDAV drive or configure Samba separately.

---

## Using real RDP / SSH too

BridgeDesk replaces the *need* to expose RDP/SSH, but it pairs well with them:

- **SSH to the PC from your phone:** open the **Terminal** tab (PowerShell) and you already have a remote shell. To reach a *different* server, just `ssh user@host` from that terminal.
- **Windows RDP:** from the PC terminal you can enable it (`Set-ItemProperty 'HKLM:\System\CurrentControlSet\Control\Terminal Server' -Name fDenyTSConnections -Value 0`) and then connect with any RDP client over the same LAN. BridgeDesk's remote desktop is a lighter alternative that needs no extra service.
- **SSH *into* the phone:** install a Termux/SSH server on Android; BridgeDesk's phone terminal (`sh`) is enough for most file/process tasks without it.

---

## Security model

- **Access key** — a 16-character key gates every connection (constant-time compared). Rotate it anytime; that disconnects all devices.
- **Pinned TLS** — the hub generates a self-signed certificate; phones and linked PCs verify it by **SHA-256 fingerprint**, so a man-in-the-middle with a different cert is rejected (verified in testing). The access key is never sent to anything but the pinned host.
- **Per-capability switches** — shell, files, screen/control and MCP can each be turned off, on both the PC and the phone.
- **Loopback MCP** — Claude's control surface is bound to 127.0.0.1 and never exposed to the network unless you add a trusted relay.
- **No cloud** — there is no account and no third-party server in the path. Your key and data stay on your devices.

Because it's self-signed and sideloaded, treat BridgeDesk as a tool for networks you trust (home/office LAN, or over your own VPN/Tailscale). It is not meant to be published to the open internet.

---

## Building from source

**Hub** (needs Go ≥ 1.22; dependencies are vendored in `hub/third_party`, so it builds offline):

```
cd hub && ./build.sh          # → ../dist/BridgeDesk.exe, bridgedesk-linux, …
```

**Android app** (needs a JDK + Android build-tools ≥ 34 and platform android-34; no Gradle):

```
cd android && ANDROID_SDK=/path/to/android-sdk ./build.sh   # → BridgeDesk.apk
```

The hub embeds the web dashboard (`hub/web/`), so the single binary serves the whole UI. The Android app is pure framework Java (no third-party libraries), which is why it builds with just the command-line tools.
```

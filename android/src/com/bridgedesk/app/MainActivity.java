package com.bridgedesk.app;

import android.app.Activity;
import android.content.Context;
import android.content.Intent;
import android.graphics.Color;
import android.net.Uri;
import android.net.http.SslCertificate;
import android.net.http.SslError;
import android.os.Build;
import android.os.Bundle;
import android.os.Environment;
import android.provider.Settings;
import android.text.InputType;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.webkit.*;
import android.widget.*;

import android.content.ContentValues;
import android.provider.MediaStore;

import java.io.File;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.URL;
import java.security.MessageDigest;
import java.security.SecureRandom;
import java.security.cert.X509Certificate;
import java.util.Locale;
import javax.net.ssl.HttpsURLConnection;
import javax.net.ssl.SSLContext;
import javax.net.ssl.TrustManager;

/**
 * Two things in one screen:
 *  - a WebView running the PC's dashboard, so you drive the PC from your phone;
 *  - controls to turn on the phone agent + grant screen/accessibility/storage,
 *    so the PC (and Claude) can drive the phone.
 */
public class MainActivity extends Activity {
    private Store store;
    private WebView web;
    private LinearLayout setup;
    private FrameLayout rootView;
    private TextView statusLine;
    private ValueCallback<Uri[]> pendingChooser;
    private static final int REQ_FILE_CHOOSER = 41;

    // ---- Aria chat ----
    private AiClient ai;
    private LinearLayout chatLog;
    private ScrollView chatScroll;
    private EditText chatInput;
    private Button sendBtn;
    private TextView ariaBubble;          // the assistant bubble currently being filled
    private boolean allowChanges = false;
    private boolean busy = false;
    private java.util.HashMap<String, TextView> fleetBubbles;
    private View ariaView, fleetView;
    private Button tabAria, tabFleet;
    private boolean fleetLoaded = false;

    @Override protected void onCreate(Bundle b) {
        super.onCreate(b);
        store = new Store(this);
        rootView = new FrameLayout(this);
        rootView.setBackgroundColor(0xFF0D1117);
        setContentView(rootView);
        requestRuntimePerms();
        if (handlePairLink(getIntent())) return;         // scanned QR → connect straight away
        if (store.host().isEmpty()) showSetup(); else showDashboard();
    }

    @Override protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        setIntent(intent);
        if (handlePairLink(intent)) toast("Connected to " + store.host());
    }

    /** Parse a bd://host:port?key=..&fp=.. pairing link from a scanned QR and connect. */
    private boolean handlePairLink(Intent intent) {
        if (intent == null || intent.getData() == null) return false;
        Uri u = intent.getData();
        if (!"bd".equalsIgnoreCase(u.getScheme())) return false;
        String host = u.getHost();
        if (host == null || host.isEmpty()) return false;
        int port = u.getPort() > 0 ? u.getPort() : 47810;
        String key = u.getQueryParameter("key");
        String fp = u.getQueryParameter("fp");
        store.setConnection(host, port, key == null ? "" : key, fp == null ? "" : fp);
        showDashboard();
        return true;
    }

    @Override protected void onResume() {
        super.onResume();
        if (statusLine != null) statusLine.setText(statusText());
    }

    // ---------- setup screen ----------
    private void showSetup() {
        rootView.removeAllViews();
        ScrollView sv = new ScrollView(this);
        setup = col();
        setup.setPadding(dp(22), dp(40), dp(22), dp(22));

        setup.addView(title("Aria"));
        setup.addView(hint("Your assistant on the phone, powered by the Aria/BridgeDesk hub on your PC. On the PC, open BridgeDesk and read the access key and one of the LAN addresses — or scan the dashboard QR."));

        final EditText host = field("PC address or IP (e.g. 192.168.1.20)", InputType.TYPE_CLASS_TEXT);
        final EditText port = field("Port (default 47810)", InputType.TYPE_CLASS_NUMBER);
        port.setText("47810");
        final EditText key = field("Access key (XXXX-XXXX-XXXX-XXXX)", InputType.TYPE_CLASS_TEXT);
        final EditText fp = field("Certificate fingerprint (optional, recommended)", InputType.TYPE_CLASS_TEXT);
        setup.addView(host); setup.addView(port); setup.addView(key); setup.addView(fp);

        Button connect = button("Connect", true);
        connect.setOnClickListener(v -> {
            String h = host.getText().toString().trim();
            if (h.isEmpty()) { toast("Enter the PC address"); return; }
            int p = 47810;
            try { p = Integer.parseInt(port.getText().toString().trim()); } catch (Exception ignored) {}
            store.setConnection(h, p, key.getText().toString().trim(), fp.getText().toString().trim());
            showDashboard();
        });
        setup.addView(connect);
        setup.addView(hint("Tip: you can paste a full link like bd://192.168.1.20:47810?key=...&fp=... into the address box."));
        host.addTextChangedListener(new SimpleWatcher(s -> maybeParseLink(s, host, port, key, fp)));

        sv.addView(setup);
        rootView.addView(sv);
    }

    private void maybeParseLink(String s, EditText host, EditText port, EditText key, EditText fp) {
        if (s == null || !s.startsWith("bd://")) return;
        try {
            Uri u = Uri.parse(s.replaceFirst("bd://", "http://"));
            host.setText(u.getHost());
            if (u.getPort() > 0) port.setText(String.valueOf(u.getPort()));
            if (u.getQueryParameter("key") != null) key.setText(u.getQueryParameter("key"));
            if (u.getQueryParameter("fp") != null) fp.setText(u.getQueryParameter("fp"));
        } catch (Exception ignored) {}
    }

    // ---------- dashboard screen (Aria chat + Fleet web) ----------
    private void showDashboard() {
        rootView.removeAllViews();
        LinearLayout colv = col();

        // toolbar: [ Aria | Fleet ]  status …  Phone access  ⚙
        LinearLayout bar = new LinearLayout(this);
        bar.setOrientation(LinearLayout.HORIZONTAL);
        bar.setBackgroundColor(0xFF161B22);
        bar.setPadding(dp(8), dp(8), dp(8), dp(8));
        bar.setGravity(Gravity.CENTER_VERTICAL);
        tabAria = smallButton("Aria", v -> selectTab(true));
        tabFleet = smallButton("Fleet", v -> selectTab(false));
        bar.addView(tabAria);
        bar.addView(space(4));
        bar.addView(tabFleet);
        statusLine = new TextView(this);
        statusLine.setTextColor(0xFF8B98A9);
        statusLine.setTextSize(TypedValue.COMPLEX_UNIT_SP, 11);
        statusLine.setGravity(Gravity.END);
        statusLine.setText(statusText());
        statusLine.setLayoutParams(new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));
        bar.addView(statusLine);
        bar.addView(space(6));
        bar.addView(smallButton("⚙", v -> showAgentPanel()));
        colv.addView(bar);

        FrameLayout content = new FrameLayout(this);
        content.setLayoutParams(new LinearLayout.LayoutParams(-1, 0, 1f));
        ariaView = buildAriaView();
        fleetView = buildFleetView();
        content.addView(ariaView);
        content.addView(fleetView);
        colv.addView(content);

        rootView.addView(colv);
        selectTab(true); // Aria is home
    }

    private void selectTab(boolean aria) {
        if (ariaView == null) return;
        ariaView.setVisibility(aria ? View.VISIBLE : View.GONE);
        fleetView.setVisibility(aria ? View.GONE : View.VISIBLE);
        tabAria.setBackgroundColor(aria ? 0xFF2D6FD6 : 0xFF1C2230);
        tabFleet.setBackgroundColor(aria ? 0xFF1C2230 : 0xFF2D6FD6);
        if (!aria && !fleetLoaded) { fleetLoaded = true; loadDashboard(); }
        if (aria && chatInput != null) chatInput.requestFocus();
    }

    // ---- Aria chat view ----
    private View buildAriaView() {
        LinearLayout v = col();
        v.setBackgroundColor(0xFF0D1117);

        chatScroll = new ScrollView(this);
        chatScroll.setLayoutParams(new LinearLayout.LayoutParams(-1, 0, 1f));
        chatScroll.setPadding(dp(12), dp(12), dp(12), dp(6));
        chatLog = col();
        chatScroll.addView(chatLog);
        v.addView(chatScroll);

        addNote("Ask Aria anything. She runs on your PC and can see your whole BridgeDesk fleet. "
                + "Reading is free; turn on “Allow changes” before asking her to run commands, "
                + "send messages, move files or control a device.");

        // fleet quick actions
        LinearLayout qa = new LinearLayout(this);
        qa.setOrientation(LinearLayout.HORIZONTAL);
        qa.setPadding(dp(10), dp(2), dp(10), dp(2));
        Button checkBtn = smallButton("Check fleet", x -> onCheckFleet());
        Button runBtn = smallButton("Run on fleet…", x -> onRunFleet());
        qa.addView(checkBtn);
        qa.addView(space(6));
        qa.addView(runBtn);
        v.addView(qa);

        // allow-changes row
        LinearLayout acRow = new LinearLayout(this);
        acRow.setOrientation(LinearLayout.HORIZONTAL);
        acRow.setGravity(Gravity.CENTER_VERTICAL);
        acRow.setPadding(dp(12), 0, dp(12), 0);
        TextView acl = new TextView(this);
        acl.setText("Allow changes (run commands, send, control)");
        acl.setTextColor(0xFF8B98A9);
        acl.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12);
        acl.setLayoutParams(new LinearLayout.LayoutParams(0, -2, 1f));
        Switch acsw = new Switch(this);
        acsw.setChecked(allowChanges);
        acsw.setOnCheckedChangeListener((b, on) -> allowChanges = on);
        acRow.addView(acl);
        acRow.addView(acsw);
        v.addView(acRow);

        // input row
        LinearLayout inRow = new LinearLayout(this);
        inRow.setOrientation(LinearLayout.HORIZONTAL);
        inRow.setGravity(Gravity.CENTER_VERTICAL);
        inRow.setPadding(dp(8), dp(4), dp(8), dp(8));
        chatInput = new EditText(this);
        chatInput.setHint("Message Aria…");
        chatInput.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_FLAG_CAP_SENTENCES | InputType.TYPE_TEXT_FLAG_MULTI_LINE);
        chatInput.setMaxLines(4);
        chatInput.setTextColor(0xFFE6EDF3);
        chatInput.setHintTextColor(0xFF6B7684);
        chatInput.setBackgroundColor(0xFF161B22);
        chatInput.setPadding(dp(12), dp(10), dp(12), dp(10));
        chatInput.setLayoutParams(new LinearLayout.LayoutParams(0, -2, 1f));
        sendBtn = button("Send", true);
        LinearLayout.LayoutParams sbp = new LinearLayout.LayoutParams(-2, -2);
        sbp.leftMargin = dp(8); sbp.topMargin = 0;
        sendBtn.setLayoutParams(sbp);
        sendBtn.setOnClickListener(x -> onSend());
        inRow.addView(chatInput);
        inRow.addView(sendBtn);
        v.addView(inRow);

        if (ai == null) ai = new AiClient(store, new ChatListener());
        return v;
    }

    private void onSend() {
        if (busy) { ai.cancel(); return; }
        String text = chatInput.getText().toString().trim();
        if (text.isEmpty()) return;
        chatInput.setText("");
        addBubble(true, text);
        ariaBubble = addBubble(false, "");
        ariaBubble.setText("…");
        setBusy(true);
        ai.ask(text, allowChanges, null);
    }

    private void setBusy(boolean b) {
        busy = b;
        if (sendBtn != null) sendBtn.setText(b ? "Stop" : "Send");
    }

    // Read-only sweep across every online device (single coordinating turn).
    private void onCheckFleet() {
        if (busy) { ai.cancel(); return; }
        addBubble(true, "Check my fleet");
        ariaBubble = addBubble(false, "…");
        setBusy(true);
        ai.ask("Check my fleet: list every online device and, for each, tell me what needs my "
                + "attention — notifications, any pending 2FA code, anything waiting. Short per-device "
                + "summary, device name as a heading.", false, null);
    }

    // Run the text in the box as a task on every online device, one worker each.
    private void onRunFleet() {
        if (busy) { ai.cancel(); return; }
        String task = chatInput.getText().toString().trim();
        if (task.isEmpty()) { toast("Type a task first, then tap Run on fleet"); chatInput.requestFocus(); return; }
        chatInput.setText("");
        addBubble(true, "Run on every device:\n" + task);
        if (!allowChanges) addNote("Reading only — turn on “Allow changes” for the workers to change anything.");
        fleetBubbles = new java.util.HashMap<>();
        setBusy(true);
        ai.askFleet(task, null, allowChanges);
    }

    /** Get or create the per-device output bubble for a fleet run (adds a heading the first time). */
    private TextView fleetBody(String device) {
        if (fleetBubbles == null) fleetBubbles = new java.util.HashMap<>();
        TextView body = fleetBubbles.get(device);
        if (body == null) {
            TextView head = new TextView(this);
            head.setText("▸ " + (device == null || device.isEmpty() ? "device" : device));
            head.setTextColor(0xFF58A6FF);
            head.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
            head.setPadding(dp(2), dp(10), dp(2), dp(2));
            chatLog.addView(head);
            body = addBubble(false, "…");
            fleetBubbles.put(device, body);
        }
        return body;
    }

    /** A chat bubble. user = right/blue, assistant = left/gray. Returns its TextView. */
    private TextView addBubble(boolean user, String text) {
        TextView t = new TextView(this);
        t.setText(text);
        t.setTextColor(user ? 0xFFFFFFFF : 0xFFE6EDF3);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        t.setBackgroundColor(user ? 0xFF2D6FD6 : 0xFF161B22);
        t.setPadding(dp(12), dp(10), dp(12), dp(10));
        t.setTextIsSelectable(true);
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(-2, -2);
        lp.gravity = user ? Gravity.END : Gravity.START;
        lp.topMargin = dp(6);
        lp.leftMargin = user ? dp(40) : 0;
        lp.rightMargin = user ? 0 : dp(40);
        t.setLayoutParams(lp);
        chatLog.addView(t);
        scrollDown();
        return t;
    }

    private void addNote(String text) {
        TextView t = new TextView(this);
        t.setText(text);
        t.setTextColor(0xFF6B7684);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12);
        t.setPadding(dp(2), dp(4), dp(2), dp(8));
        chatLog.addView(t);
    }

    private void addToolChip(String name) {
        TextView t = new TextView(this);
        t.setText("⚙ " + name);
        t.setTextColor(0xFF8B98A9);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12);
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(-2, -2);
        lp.topMargin = dp(4);
        t.setLayoutParams(lp);
        chatLog.addView(t);
        scrollDown();
    }

    private void scrollDown() {
        if (chatScroll != null) chatScroll.post(() -> chatScroll.fullScroll(View.FOCUS_DOWN));
    }

    /** Streams one turn from the hub relay onto the chat, all on the UI thread. */
    private class ChatListener implements AiClient.Listener {
        public void onDelta(final String text) {
            runOnUiThread(() -> {
                if (ariaBubble == null) ariaBubble = addBubble(false, "");
                String cur = ariaBubble.getText().toString();
                if (cur.equals("…")) cur = "";
                ariaBubble.setText(cur.isEmpty() ? text : cur + "\n" + text);
                scrollDown();
            });
        }
        public void onTool(final String name) {
            runOnUiThread(() -> { addToolChip(name); ariaBubble = null; });
        }
        public void onDone(final String session, final String text) {
            runOnUiThread(() -> {
                if (ariaBubble != null && "…".equals(ariaBubble.getText().toString())) {
                    ariaBubble.setText(text == null || text.isEmpty() ? "(no reply)" : text);
                }
                ariaBubble = null;
                setBusy(false);
                scrollDown();
            });
        }
        public void onError(final String message) {
            runOnUiThread(() -> {
                if (ariaBubble != null && "…".equals(ariaBubble.getText().toString())) chatLog.removeView(ariaBubble);
                ariaBubble = null;
                TextView t = addBubble(false, "⚠ " + message);
                t.setTextColor(0xFFF0883E);
                setBusy(false);
            });
        }
        public void onStatus(final String status) {
            runOnUiThread(() -> { if (statusLine != null) statusLine.setText("PC " + store.host() + "  ·  Aria " + status); });
        }
        public void onFleetStart(final String device) {
            runOnUiThread(() -> fleetBody(device));
        }
        public void onFleetDelta(final String device, final String text) {
            runOnUiThread(() -> {
                TextView b = fleetBody(device);
                String cur = b.getText().toString();
                if (cur.equals("…")) cur = "";
                b.setText(cur.isEmpty() ? text : cur + "\n" + text);
                scrollDown();
            });
        }
        public void onFleetTool(final String device, final String name) {
            runOnUiThread(() -> {
                TextView b = fleetBody(device);
                String cur = b.getText().toString();
                if (cur.equals("…")) cur = "";
                b.setText((cur.isEmpty() ? "" : cur + "\n") + "⚙ " + name);
                scrollDown();
            });
        }
        public void onFleetDeviceDone(final String device, final boolean ok, final String text, final String error) {
            runOnUiThread(() -> {
                TextView b = fleetBody(device);
                if (!ok && error != null && !error.isEmpty()) {
                    b.setText("⚠ " + error);
                    b.setTextColor(0xFFF0883E);
                } else if ("…".equals(b.getText().toString())) {
                    b.setText(text == null || text.isEmpty() ? "(no output)" : text);
                }
                scrollDown();
            });
        }
        public void onFleetDone(final org.json.JSONArray devices) {
            runOnUiThread(() -> {
                int n = devices == null ? 0 : devices.length();
                addNote("Finished across " + n + " device" + (n == 1 ? "" : "s") + ".");
                fleetBubbles = null;
                setBusy(false);
                scrollDown();
            });
        }
    }

    // ---- Fleet (PC dashboard) view ----
    private View buildFleetView() {
        web = new WebView(this);
        WebSettings ws = web.getSettings();
        ws.setJavaScriptEnabled(true);
        ws.setDomStorageEnabled(true);
        ws.setMediaPlaybackRequiresUserGesture(false);
        ws.setBuiltInZoomControls(true);
        ws.setDisplayZoomControls(false);
        web.setWebViewClient(new PinnedWebViewClient());
        web.setWebChromeClient(new WebChromeClient() {
            @Override public boolean onShowFileChooser(WebView v, ValueCallback<Uri[]> cb, FileChooserParams params) {
                if (pendingChooser != null) pendingChooser.onReceiveValue(null);
                pendingChooser = cb;
                try {
                    Intent i = params.createIntent();
                    i.putExtra(Intent.EXTRA_ALLOW_MULTIPLE, params.getMode() == FileChooserParams.MODE_OPEN_MULTIPLE);
                    startActivityForResult(i, REQ_FILE_CHOOSER);
                } catch (Exception e) {
                    pendingChooser = null; toast("No file picker available"); return false;
                }
                return true;
            }
        });
        web.setDownloadListener((url, ua, contentDisposition, mime, length) ->
                downloadToPhone(url, contentDisposition, mime));
        return web;
    }

    private void loadDashboard() {
        final String base = "https://" + store.host() + ":" + store.port() + "/?key=" + Uri.encode(store.key());
        if (!store.googleConfigured()) { web.loadUrl(base); return; }
        // Fetching/refreshing the Google token may touch the network — do it off the UI thread,
        // then load with ?gid so the hub sets its cookie for the WebView's later requests.
        new Thread(() -> {
            String gid = GoogleAuth.freshIdToken(store);
            final String url = gid.isEmpty() ? base : base + "&gid=" + Uri.encode(gid);
            runOnUiThread(() -> { if (web != null) web.loadUrl(url); });
        }, "dash-gid").start();
    }

    private String statusText() {
        String conn = AgentService.connected ? "phone shared ✓" : (store.agentEnabled() ? "linking…" : "phone not shared");
        return "PC " + store.host() + "   ·   " + conn;
    }

    // ---------- phone-access panel ----------
    private void showAgentPanel() {
        ScrollView sv = new ScrollView(this);
        LinearLayout c = col();
        c.setPadding(dp(20), dp(20), dp(20), dp(20));
        c.setBackgroundColor(0xFF0D1117);
        c.addView(title("Let the PC use this phone"));
        c.addView(hint("Turn these on so your PC (and Claude) can view and control this phone. You can stop anytime from the notification."));

        // --- Google sign-in (needed when the hub requires it, e.g. over the internet) ---
        c.addView(sectionLabel("Google account"));
        c.addView(hint("If the hub requires Google sign-in, connect the account that's allowed on the PC. "
                + "Paste your Google OAuth client ID (and secret, for a Desktop-type client)."));
        final EditText gid = field("Google OAuth client ID", InputType.TYPE_CLASS_TEXT);
        gid.setText(store.gClientId());
        final EditText gsec = field("Client secret (Desktop client; optional)", InputType.TYPE_CLASS_TEXT);
        gsec.setText(store.gClientSecret());
        c.addView(gid); c.addView(gsec);
        final TextView gstatus = hint(store.gEmail().isEmpty() ? "Not signed in." : "Signed in as " + store.gEmail());
        c.addView(actionRow("Sign in with Google", "Opens Google in your browser.",
                store.gEmail().isEmpty() ? "Sign in" : "Re-sign in", v -> {
            store.setGoogleClient(gid.getText().toString(), gsec.getText().toString());
            gstatus.setText("Opening Google…");
            GoogleAuth.signIn(this, store, new GoogleAuth.SignInCb() {
                public void onResult(String email) { runOnUiThread(() -> { gstatus.setText("Signed in as " + email); toast("Signed in as " + email); }); }
                public void onError(String msg) { runOnUiThread(() -> { gstatus.setText("Sign-in failed: " + msg); toast("Sign-in failed: " + msg); }); }
            });
        }));
        c.addView(gstatus);
        if (!store.gEmail().isEmpty()) {
            c.addView(actionRow("Sign out of Google", "Forget the saved Google tokens on this phone.",
                    "Sign out", v -> { store.clearGoogle(); toast("Signed out"); showAgentPanel(); }));
        }
        c.addView(sectionLabel("Phone sharing & control"));

        c.addView(toggleRow("Share this phone with the PC", store.agentEnabled(), on -> {
            if (on) { store.setAgentEnabled(true); AgentService.startAgent(this); }
            else { store.setAgentEnabled(false); AgentService.stopAgent(this); }
        }));
        c.addView(actionRow("Allow screen capture", "Lets the PC see this screen (works while locked).",
                "Grant", v -> requestProjection()));
        c.addView(actionRow("Allow remote touch & typing", "Turn on the BridgeDesk accessibility service.",
                ControlService.available() ? "Enabled ✓" : "Open settings", v -> openAccessibility()));
        c.addView(actionRow("Mirror notifications to PC", "Forward messages and push prompts (SMS, chat, Okta Verify). You can approve them from the PC.",
                NotificationService.isEnabled(this) ? "Enabled ✓" : "Open settings", v -> openNotificationAccess()));
        if (Build.VERSION.SDK_INT >= 30) {
            c.addView(actionRow("Full file access", "Needed to browse all of internal storage.",
                    hasAllFiles() ? "Granted ✓" : "Grant", v -> requestAllFiles()));
        }
        c.addView(actionRow("Calls & SMS", "See calls and texts on the PC: answer/decline, call back, read and send SMS, send WhatsApp.",
                PhoneBridge.allGranted(this) ? "Granted ✓" : "Grant", v -> requestPermissions(PhoneBridge.PERMS, 8)));
        c.addView(hint("Clipboard: text you copy on a PC lands here automatically. To send this phone's clipboard to your PCs, tap \"Send clipboard to PC\" on the BridgeDesk notification, use Share → Send to PC, or just open BridgeDesk."));
        c.addView(toggleRow("Allow shell commands", store.allowShell(), on -> store.setAllow("allowShell", on)));
        c.addView(toggleRow("Allow file access", store.allowFiles(), on -> store.setAllow("allowFiles", on)));
        c.addView(toggleRow("Allow screen & control", store.allowControl(), on -> store.setAllow("allowControl", on)));
        c.addView(toggleRow("Allow notification mirroring", store.allowNotifications(), on -> store.setAllow("allowNotifications", on)));
        c.addView(toggleRow("Allow calls & SMS", store.allowPhone(), on -> store.setAllow("allowPhone", on)));
        c.addView(toggleRow("Sync clipboard with PCs", store.allowClipSync(), on -> store.setAllow("allowClipSync", on)));

        Button back = button("Done", true);
        back.setOnClickListener(v -> { showDashboard(); });
        c.addView(back);
        Button changePc = button("Change PC / disconnect", false);
        changePc.setOnClickListener(v -> {
            if (ai != null) { ai.close(); ai = null; }
            fleetLoaded = false;
            store.setConnection("", store.port(), "", "");
            showSetup();
        });
        c.addView(changePc);
        sv.addView(c);
        rootView.removeAllViews();
        rootView.addView(sv);
    }

    private void requestProjection() {
        if (!store.agentEnabled()) { store.setAgentEnabled(true); AgentService.startAgent(this); }
        startActivity(new Intent(this, ProjectionRequestActivity.class));
    }

    private void openAccessibility() {
        try { startActivity(new Intent(Settings.ACTION_ACCESSIBILITY_SETTINGS)); }
        catch (Exception e) { toast("Open Settings → Accessibility → BridgeDesk"); }
    }

    private void openNotificationAccess() {
        try { startActivity(new Intent(Settings.ACTION_NOTIFICATION_LISTENER_SETTINGS)); }
        catch (Exception e) { toast("Open Settings → Notification access → BridgeDesk"); }
    }

    private boolean hasAllFiles() {
        return Build.VERSION.SDK_INT >= 30 && Environment.isExternalStorageManager();
    }
    private void requestAllFiles() {
        if (Build.VERSION.SDK_INT < 30) return;
        try {
            Intent i = new Intent(Settings.ACTION_MANAGE_APP_ALL_FILES_ACCESS_PERMISSION, Uri.parse("package:" + getPackageName()));
            startActivity(i);
        } catch (Exception e) {
            startActivity(new Intent(Settings.ACTION_MANAGE_ALL_FILES_ACCESS_PERMISSION));
        }
    }

    private void requestRuntimePerms() {
        if (Build.VERSION.SDK_INT >= 33) {
            try { requestPermissions(new String[]{"android.permission.POST_NOTIFICATIONS"}, 7); } catch (Exception ignored) {}
        }
    }

    @Override protected void onActivityResult(int req, int res, Intent data) {
        super.onActivityResult(req, res, data);
        if (req != REQ_FILE_CHOOSER || pendingChooser == null) return;
        Uri[] out = null;
        if (res == RESULT_OK && data != null) {
            if (data.getClipData() != null) {
                int n = data.getClipData().getItemCount();
                out = new Uri[n];
                for (int i = 0; i < n; i++) out[i] = data.getClipData().getItemAt(i).getUri();
            } else if (data.getData() != null) {
                out = new Uri[]{ data.getData() };
            }
        }
        pendingChooser.onReceiveValue(out);
        pendingChooser = null;
    }

    // ---------- downloads (PC -> phone) ----------
    /** Fetch the file over the pinned TLS connection and save it to Downloads/BridgeDesk. */
    private void downloadToPhone(final String url, final String contentDisposition, final String mime) {
        if (url.startsWith("data:")) { saveDataUrl(url); return; } // e.g. Desktop → screenshot button
        final String name = fileNameFor(url, contentDisposition, mime);
        toast("Downloading " + name + "…");
        new Thread(() -> {
            HttpsURLConnection c = null;
            try {
                SSLContext ctx = SSLContext.getInstance("TLS");
                ctx.init(null, new TrustManager[]{ new WsClient.PinnedTrust(store.fingerprint()) }, new SecureRandom());
                c = (HttpsURLConnection) new URL(url).openConnection();
                c.setSSLSocketFactory(ctx.getSocketFactory());
                c.setHostnameVerifier((h, sess) -> true); // identity is the pinned fingerprint, not the hostname
                c.setConnectTimeout(15000);
                c.setReadTimeout(0);
                int code = c.getResponseCode();
                if (code != 200) throw new Exception("HTTP " + code);
                String where;
                try (InputStream in = c.getInputStream()) {
                    where = saveToDownloads(name, in);
                }
                final String msg = "Saved to " + where;
                runOnUiThread(() -> Toast.makeText(this, msg, Toast.LENGTH_LONG).show());
            } catch (Exception e) {
                final String msg = "Download failed: " + e.getMessage();
                runOnUiThread(() -> Toast.makeText(this, msg, Toast.LENGTH_LONG).show());
            } finally {
                if (c != null) c.disconnect();
            }
        }, "bd-download").start();
    }

    private void saveDataUrl(final String url) {
        new Thread(() -> {
            try {
                int comma = url.indexOf(',');
                String meta = url.substring(5, comma);
                byte[] bytes = meta.endsWith(";base64")
                        ? android.util.Base64.decode(url.substring(comma + 1), android.util.Base64.DEFAULT)
                        : Uri.decode(url.substring(comma + 1)).getBytes("UTF-8");
                String ext = meta.startsWith("image/png") ? ".png" : meta.startsWith("image/") ? ".jpg" : ".bin";
                String name = "BridgeDesk-" + new java.text.SimpleDateFormat("yyyyMMdd-HHmmss", Locale.US).format(new java.util.Date()) + ext;
                final String where = saveToDownloads(name, new java.io.ByteArrayInputStream(bytes));
                runOnUiThread(() -> Toast.makeText(this, "Saved to " + where, Toast.LENGTH_LONG).show());
            } catch (Exception e) {
                runOnUiThread(() -> Toast.makeText(this, "Save failed: " + e.getMessage(), Toast.LENGTH_LONG).show());
            }
        }, "bd-save").start();
    }

    private String fileNameFor(String url, String cd, String mime) {
        try {
            String p = Uri.parse(url).getQueryParameter("path");
            if (p != null && !p.isEmpty()) {
                String n = p.substring(Math.max(p.lastIndexOf('/'), p.lastIndexOf('\\')) + 1);
                if (!n.isEmpty()) return n;
            }
        } catch (Exception ignored) {}
        return URLUtil.guessFileName(url, cd, mime);
    }

    private String saveToDownloads(String name, InputStream in) throws Exception {
        byte[] buf = new byte[64 * 1024];
        int r;
        if (Build.VERSION.SDK_INT >= 29) {
            ContentValues v = new ContentValues();
            v.put(MediaStore.MediaColumns.DISPLAY_NAME, name);
            v.put(MediaStore.MediaColumns.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS + "/BridgeDesk");
            v.put(MediaStore.MediaColumns.IS_PENDING, 1);
            Uri item = getContentResolver().insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, v);
            if (item == null) throw new Exception("could not create file in Downloads");
            try (OutputStream out = getContentResolver().openOutputStream(item)) {
                while ((r = in.read(buf)) > 0) out.write(buf, 0, r);
            } catch (Exception e) {
                getContentResolver().delete(item, null, null);
                throw e;
            }
            v.clear();
            v.put(MediaStore.MediaColumns.IS_PENDING, 0);
            getContentResolver().update(item, v, null, null);
            return "Downloads/BridgeDesk/" + name;
        }
        File dir = new File(Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_DOWNLOADS), "BridgeDesk");
        dir.mkdirs();
        File f = new File(dir, name);
        try (OutputStream out = new FileOutputStream(f)) {
            while ((r = in.read(buf)) > 0) out.write(buf, 0, r);
        }
        return "Downloads/BridgeDesk/" + name;
    }

    // ---------- WebView TLS pinning ----------
    private class PinnedWebViewClient extends WebViewClient {
        @Override public void onReceivedSslError(WebView view, SslErrorHandler handler, SslError error) {
            String want = store.fingerprint().replace(" ", "").toUpperCase(Locale.US);
            if (want.isEmpty()) { handler.proceed(); return; } // key still gates access on the LAN
            try {
                if (Build.VERSION.SDK_INT >= 29) {
                    SslCertificate cert = error.getCertificate();
                    X509Certificate x = cert.getX509Certificate();
                    if (x != null) {
                        MessageDigest md = MessageDigest.getInstance("SHA-256");
                        byte[] d = md.digest(x.getEncoded());
                        StringBuilder sb = new StringBuilder();
                        for (byte bb : d) sb.append(String.format("%02X", bb));
                        if (sb.toString().equals(want)) { handler.proceed(); return; }
                    }
                } else { handler.proceed(); return; }
            } catch (Exception ignored) {}
            handler.cancel();
            toast("Certificate did not match the pinned fingerprint");
        }
        @Override public boolean shouldOverrideUrlLoading(WebView v, WebResourceRequest r) { return false; }
    }

    @Override public void onRequestPermissionsResult(int req, String[] perms, int[] res) {
        super.onRequestPermissionsResult(req, perms, res);
        if (req == 8) {
            toast(PhoneBridge.allGranted(this) ? "Calls & SMS ready" : "Some permissions were denied — if Android blocks them, open App info → ⋮ → Allow restricted settings");
            showAgentPanel();
        }
    }

    // Opening BridgeDesk is a natural moment to sync: Android only lets the focused app read the clipboard.
    @Override public void onWindowFocusChanged(boolean hasFocus) {
        super.onWindowFocusChanged(hasFocus);
        if (!hasFocus) return;
        String t = ClipSendActivity.readClipboard(this);
        if (!t.isEmpty() && !t.equals(Rpc.lastClip)) ClipSendActivity.sendToPc(t);
    }

    @Override public void onBackPressed() {
        if (fleetView != null && fleetView.getVisibility() == View.VISIBLE && web != null && web.canGoBack()) web.goBack();
        else super.onBackPressed();
    }

    @Override protected void onDestroy() {
        if (ai != null) { ai.close(); ai = null; }
        super.onDestroy();
    }

    // ---------- tiny UI helpers ----------
    private LinearLayout col() { LinearLayout l = new LinearLayout(this); l.setOrientation(LinearLayout.VERTICAL); l.setLayoutParams(new ViewGroup.LayoutParams(-1, -1)); return l; }
    private TextView title(String t) { TextView v = new TextView(this); v.setText(t); v.setTextColor(0xFFE6EDF3); v.setTextSize(TypedValue.COMPLEX_UNIT_SP, 24); v.setPadding(0, 0, 0, dp(10)); return v; }
    private TextView hint(String t) { TextView v = new TextView(this); v.setText(t); v.setTextColor(0xFF8B98A9); v.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13); v.setPadding(0, dp(4), 0, dp(14)); return v; }
    private TextView sectionLabel(String t) { TextView v = new TextView(this); v.setText(t); v.setTextColor(0xFF58A6FF); v.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13); v.setPadding(0, dp(14), 0, dp(4)); return v; }
    private EditText field(String hintTxt, int type) {
        EditText e = new EditText(this); e.setHint(hintTxt); e.setInputType(type);
        e.setTextColor(0xFFE6EDF3); e.setHintTextColor(0xFF6B7684); e.setBackgroundColor(0xFF161B22);
        e.setPadding(dp(12), dp(12), dp(12), dp(12));
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(-1, -2); lp.bottomMargin = dp(10); e.setLayoutParams(lp);
        return e;
    }
    private Button button(String t, boolean primary) {
        Button b = new Button(this); b.setText(t); b.setAllCaps(false);
        b.setTextColor(primary ? 0xFFFFFFFF : 0xFFE6EDF3); b.setBackgroundColor(primary ? 0xFF2D6FD6 : 0xFF1C2230);
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(-1, -2); lp.topMargin = dp(8); b.setLayoutParams(lp);
        return b;
    }
    private Button smallButton(String t, View.OnClickListener l) {
        Button b = new Button(this); b.setText(t); b.setAllCaps(false); b.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        b.setTextColor(0xFFE6EDF3); b.setBackgroundColor(0xFF1C2230); b.setPadding(dp(10), dp(6), dp(10), dp(6));
        b.setOnClickListener(l); return b;
    }
    private View space(int w) { View v = new View(this); v.setLayoutParams(new LinearLayout.LayoutParams(dp(w), 1)); return v; }
    private View toggleRow(String label, boolean on, final BoolSetter setter) {
        LinearLayout row = new LinearLayout(this); row.setOrientation(LinearLayout.HORIZONTAL); row.setGravity(Gravity.CENTER_VERTICAL);
        row.setPadding(0, dp(10), 0, dp(10));
        TextView t = new TextView(this); t.setText(label); t.setTextColor(0xFFE6EDF3); t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        t.setLayoutParams(new LinearLayout.LayoutParams(0, -2, 1f));
        Switch sw = new Switch(this); sw.setChecked(on);
        sw.setOnCheckedChangeListener((b, v) -> setter.set(v));
        row.addView(t); row.addView(sw);
        return row;
    }
    private View actionRow(String label, String sub, String btn, View.OnClickListener l) {
        LinearLayout row = new LinearLayout(this); row.setOrientation(LinearLayout.HORIZONTAL); row.setGravity(Gravity.CENTER_VERTICAL);
        row.setPadding(0, dp(10), 0, dp(10));
        LinearLayout tc = new LinearLayout(this); tc.setOrientation(LinearLayout.VERTICAL);
        tc.setLayoutParams(new LinearLayout.LayoutParams(0, -2, 1f));
        TextView t = new TextView(this); t.setText(label); t.setTextColor(0xFFE6EDF3); t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 15);
        TextView s = new TextView(this); s.setText(sub); s.setTextColor(0xFF8B98A9); s.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12);
        tc.addView(t); tc.addView(s);
        Button b = smallButton(btn, l);
        row.addView(tc); row.addView(b);
        return row;
    }
    private int dp(int d) { return (int) (d * getResources().getDisplayMetrics().density); }
    private void toast(String s) { Toast.makeText(this, s, Toast.LENGTH_SHORT).show(); }

    interface BoolSetter { void set(boolean v); }
    static class SimpleWatcher implements android.text.TextWatcher {
        interface Cb { void on(String s); } final Cb cb;
        SimpleWatcher(Cb cb) { this.cb = cb; }
        public void beforeTextChanged(CharSequence s, int a, int b, int c) {}
        public void onTextChanged(CharSequence s, int a, int b, int c) {}
        public void afterTextChanged(android.text.Editable e) { cb.on(e.toString()); }
    }
}

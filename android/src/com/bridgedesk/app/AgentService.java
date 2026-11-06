package com.bridgedesk.app;

import android.app.*;
import android.content.*;
import android.content.pm.ServiceInfo;
import android.media.projection.MediaProjection;
import android.media.projection.MediaProjectionManager;
import android.os.*;
import android.util.Base64;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.*;
import java.util.concurrent.*;

/**
 * Foreground service that links this phone to the PC hub as a remote device.
 * It dials out to wss://host:port/link, announces its capabilities, and answers RPCs
 * (shell, files, screen capture, input, clipboard) so the PC — and Claude via MCP — can drive it.
 */
public class AgentService extends Service {
    public static final String ACTION_START = "start";
    public static final String ACTION_STOP = "stop";
    public static final String ACTION_PROJECTION = "projection";
    private static final String CHAN = "bridgedesk";

    static volatile AgentService instance;
    static volatile boolean connected = false;

    private Store store;
    private WsClient ws;
    private ScreenCapture capture;
    private PhoneBridge phone;
    private final ExecutorService pool = Executors.newCachedThreadPool();
    private volatile boolean wantRun = false;
    private Handler main;

    // media projection result, delivered from ProjectionRequestActivity
    private int projResult; private Intent projData;

    @Override public void onCreate() {
        super.onCreate();
        instance = this;
        store = new Store(this);
        capture = new ScreenCapture(this);
        phone = new PhoneBridge(this);
        phone.start();
        main = new Handler(Looper.getMainLooper());
        createChannel();
    }

    @Override public int onStartCommand(Intent intent, int flags, int startId) {
        String action = intent == null ? ACTION_START : intent.getAction();
        if (ACTION_STOP.equals(action)) {
            wantRun = false;
            stopEverything();
            stopForeground(true);
            stopSelf();
            return START_NOT_STICKY;
        }
        if (ACTION_PROJECTION.equals(action) && intent != null) {
            projResult = intent.getIntExtra("code", 0);
            projData = intent.getParcelableExtra("data");
            startProjection();
            return START_STICKY;
        }
        startForeground(1, buildNotification("Starting…"),
                ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC);
        wantRun = true;
        store.setAgentEnabled(true);
        pool.execute(this::connectLoop);
        return START_STICKY;
    }

    private void connectLoop() {
        while (wantRun) {
            try {
                String host = store.host();
                if (host.isEmpty()) { sleep(3000); continue; }
                ws = new WsClient();
                final Object done = new Object();
                ws.connect(host, store.port(), "/link?key=" + enc(store.key()), store.fingerprint(), new WsClient.Listener() {
                    public void onOpen() {
                        connected = true;
                        updateNote("Connected to " + store.host());
                        try { ws.sendText(hello().toString()); } catch (Exception ignored) {}
                    }
                    public void onText(String msg) { pool.execute(() -> handle(msg)); }
                    public void onClose(String reason) {
                        connected = false;
                        updateNote("Reconnecting…");
                        synchronized (done) { done.notifyAll(); }
                    }
                });
            } catch (Exception e) {
                connected = false;
                updateNote("Reconnecting… (" + e.getMessage() + ")");
            }
            sleep(3000);
        }
    }

    private JSONObject hello() throws Exception {
        JSONObject info = new JSONObject();
        info.put("id", store.deviceId());
        info.put("name", store.deviceName());
        info.put("kind", "android");
        info.put("os", "Android " + Build.VERSION.RELEASE);
        info.put("platform", Build.MANUFACTURER + " " + Build.MODEL);
        info.put("home", "/storage/emulated/0");
        JSONArray caps = new JSONArray();
        if (store.allowShell()) caps.put("shell");
        if (store.allowFiles()) caps.put("files");
        if (store.allowControl()) { caps.put("screen"); caps.put("input"); }
        if (store.allowNotifications() && NotificationService.isEnabled(this)) caps.put("notifications");
        caps.put("clipboard");
        if (store.allowClipSync()) caps.put("clipsync");
        if (store.allowPhone() && PhoneBridge.anyGranted(this)) caps.put("phone");
        info.put("caps", caps);
        JSONObject root = new JSONObject();
        root.put("hello", info);
        return root;
    }

    // ---- RPC dispatch ----
    private void handle(String msg) {
        JSONObject m;
        try { m = new JSONObject(msg); } catch (Exception e) { return; }
        if (m.has("welcome")) return;
        long id = m.optLong("id", 0);
        String method = m.optString("method", "");
        if (method.isEmpty()) return;
        JSONObject params = m.optJSONObject("params");
        if (params == null) params = new JSONObject();
        try {
            Object result = Rpc.call(this, method, params);
            JSONObject out = new JSONObject();
            out.put("id", id);
            out.put("result", result == null ? JSONObject.NULL : result);
            ws.sendText(out.toString());
        } catch (Exception e) {
            try {
                JSONObject out = new JSONObject();
                out.put("id", id);
                out.put("error", e.getMessage() == null ? e.toString() : e.getMessage());
                ws.sendText(out.toString());
            } catch (Exception ignored) {}
        }
    }

    void emit(String event, JSONObject params) {
        try {
            JSONObject out = new JSONObject();
            out.put("event", event);
            out.put("params", params);
            if (ws != null) ws.sendText(out.toString());
        } catch (Exception ignored) {}
    }

    // ---- screen projection ----
    void ensureProjection() {
        if (capture.running()) return;
        if (projData == null) {
            // ask the user via a transparent activity
            Intent i = new Intent(this, ProjectionRequestActivity.class);
            i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
            startActivity(i);
        } else {
            startProjection();
        }
    }

    private void startProjection() {
        main.post(() -> {
            try {
                startForeground(1, buildNotification("Sharing screen"),
                        ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PROJECTION | ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC);
                MediaProjectionManager mpm = (MediaProjectionManager) getSystemService(MEDIA_PROJECTION_SERVICE);
                MediaProjection mp = mpm.getMediaProjection(projResult, projData);
                mp.registerCallback(new MediaProjection.Callback() {
                    public void onStop() { capture.stop(); }
                }, main);
                capture.start(mp);
            } catch (Exception ignored) {}
        });
    }

    ScreenCapture capture() { return capture; }
    Store store() { return store; }

    // ---- lifecycle / notification ----
    private void stopEverything() {
        connected = false;
        try { if (ws != null) ws.close(); } catch (Exception ignored) {}
        capture.stop();
    }

    @Override public void onDestroy() {
        wantRun = false;
        store.setAgentEnabled(false);
        stopEverything();
        if (phone != null) phone.stop();
        instance = null;
        super.onDestroy();
    }

    @Override public IBinder onBind(Intent intent) { return null; }

    private void createChannel() {
        NotificationManager nm = getSystemService(NotificationManager.class);
        NotificationChannel ch = new NotificationChannel(CHAN, "BridgeDesk", NotificationManager.IMPORTANCE_LOW);
        ch.setDescription("Keeps your phone reachable from your PC");
        nm.createNotificationChannel(ch);
    }

    private Notification buildNotification(String text) {
        Intent open = new Intent(this, MainActivity.class);
        PendingIntent pi = PendingIntent.getActivity(this, 0, open, PendingIntent.FLAG_IMMUTABLE);
        Intent stop = new Intent(this, AgentService.class).setAction(ACTION_STOP);
        PendingIntent sp = PendingIntent.getService(this, 1, stop, PendingIntent.FLAG_IMMUTABLE);
        Intent clip = new Intent(this, ClipSendActivity.class).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
        PendingIntent cp = PendingIntent.getActivity(this, 2, clip, PendingIntent.FLAG_IMMUTABLE);
        return new Notification.Builder(this, CHAN)
                .setContentTitle("BridgeDesk")
                .setContentText(text)
                .setSmallIcon(android.R.drawable.stat_sys_data_bluetooth)
                .setContentIntent(pi)
                .addAction(new Notification.Action.Builder(null, "Send clipboard to PC", cp).build())
                .addAction(new Notification.Action.Builder(null, "Stop", sp).build())
                .setOngoing(true)
                .build();
    }

    private void updateNote(String text) {
        NotificationManager nm = getSystemService(NotificationManager.class);
        try { nm.notify(1, buildNotification(text)); } catch (Exception ignored) {}
    }

    private static void sleep(long ms) { try { Thread.sleep(ms); } catch (InterruptedException ignored) {} }
    private static String enc(String s) { try { return java.net.URLEncoder.encode(s, "UTF-8"); } catch (Exception e) { return s; } }

    static String b64(byte[] b) { return Base64.encodeToString(b, Base64.NO_WRAP); }
    static byte[] unb64(String s) { return Base64.decode(s, Base64.DEFAULT); }

    public static void startAgent(Context c) {
        Intent i = new Intent(c, AgentService.class).setAction(ACTION_START);
        if (Build.VERSION.SDK_INT >= 26) c.startForegroundService(i); else c.startService(i);
    }
    public static void stopAgent(Context c) {
        c.startService(new Intent(c, AgentService.class).setAction(ACTION_STOP));
    }
}

package com.bridgedesk.app;

import org.json.JSONArray;
import org.json.JSONObject;

import java.util.List;

/**
 * Talks to the hub's "Aria" relay over the same dashboard channel the web UI uses
 * (wss://host:port/ws?key=…, TLS pinned by {@link WsClient}). It sends one prompt as an
 * {@code ai.ask} message and streams the reply back: {@code ai.delta} text chunks,
 * {@code ai.tool} notices (a fleet tool the PC is about to run), then a final result that
 * carries the resume session id.
 *
 * The hub runs the user's local Claude CLI on the PC to answer — the phone holds no key.
 */
public class AiClient {

    public interface Listener {
        void onDelta(String text);          // a chunk of assistant text (single-brain chat)
        void onTool(String name);           // the assistant is running fleet tool <name>
        void onToolResult(String device, String text); // a short result snippet from a tool (device "" = single-brain)
        void onApproval(String id, String tool, String inputJson); // PC asks Allow/Deny for a change
        void onDone(String session, String text); // turn finished; session = resume id
        void onError(String message);
        void onStatus(String status);       // "connecting", "connected", "disconnected"
        // whole-fleet run (ai.fleet): one worker per device, tagged by device name
        void onFleetStart(String device);
        void onFleetDelta(String device, String text);
        void onFleetTool(String device, String name);
        void onFleetDeviceDone(String device, boolean ok, String text, String error);
        void onFleetDone(JSONArray devices);
    }

    private final Store store;
    private final Listener listener;
    private WsClient ws;
    private volatile boolean connecting = false;
    private long reqId = 0;
    private long currentId = -1;
    private String session = "";

    // one pending prompt may be waiting for the socket to come up
    private JSONObject pending;

    public AiClient(Store store, Listener listener) {
        this.store = store;
        this.listener = listener;
    }

    public String session() { return session; }
    public void resetSession() { session = ""; }

    /** Send a prompt. Connects first if needed; the reply arrives through the listener. */
    public synchronized void ask(String prompt, boolean allowChanges, String model) {
        JSONObject params = new JSONObject();
        try {
            params.put("prompt", prompt);
            params.put("session", session);
            params.put("allowChanges", allowChanges);
            if (model != null && !model.isEmpty()) params.put("model", model);
        } catch (Exception ignored) {}

        JSONObject msg = new JSONObject();
        try {
            currentId = ++reqId;
            msg.put("action", "ai.ask");
            msg.put("id", currentId);
            msg.put("params", params);
        } catch (Exception ignored) {}

        if (ws != null && ws.isOpen()) {
            trySend(msg);
        } else {
            pending = msg;
            connect();
        }
    }

    /** Run one task on each target device (empty list = all online) as its own worker. */
    public synchronized void askFleet(String prompt, List<String> devices, boolean allowChanges) {
        JSONObject params = new JSONObject();
        try {
            params.put("prompt", prompt);
            params.put("allowChanges", allowChanges);
            JSONArray arr = new JSONArray();
            if (devices != null) for (String d : devices) arr.put(d);
            params.put("devices", arr);
        } catch (Exception ignored) {}

        JSONObject msg = new JSONObject();
        try {
            currentId = ++reqId;
            msg.put("action", "ai.fleet");
            msg.put("id", currentId);
            msg.put("params", params);
        } catch (Exception ignored) {}

        if (ws != null && ws.isOpen()) trySend(msg);
        else { pending = msg; connect(); }
    }

    /** Answer an approval card: allow or deny the pending change. */
    public synchronized void approve(String id, boolean allow) {
        if (ws == null || !ws.isOpen()) return;
        try {
            JSONObject params = new JSONObject().put("id", id).put("allow", allow);
            JSONObject msg = new JSONObject().put("action", "ai.approve").put("id", ++reqId).put("params", params);
            ws.sendText(msg.toString());
        } catch (Exception ignored) {}
    }

    /** Ask the hub to kill the in-flight CLI turn. */
    public synchronized void cancel() {
        if (ws != null && ws.isOpen()) {
            try {
                JSONObject msg = new JSONObject();
                msg.put("action", "ai.cancel");
                msg.put("id", ++reqId);
                ws.sendText(msg.toString());
            } catch (Exception ignored) {}
        }
    }

    public synchronized void close() {
        pending = null;
        if (ws != null) { try { ws.close(); } catch (Exception ignored) {} ws = null; }
    }

    private void connect() {
        if (connecting) return;
        connecting = true;
        listener.onStatus("connecting");
        final String host = store.host();
        final int port = store.port();
        final String fp = store.fingerprint();
        new Thread(() -> {
            try {
                String path = "/ws?key=" + enc(store.key());
                String gid = GoogleAuth.freshIdToken(store);
                if (!gid.isEmpty()) path += "&gid=" + enc(gid);
                WsClient c = new WsClient();
                this.ws = c;
                c.connect(host, port, path, fp, new WsClient.Listener() {
                    public void onOpen() {
                        connecting = false;
                        listener.onStatus("connected");
                        JSONObject p;
                        synchronized (AiClient.this) { p = pending; pending = null; }
                        if (p != null) trySend(p);
                    }
                    public void onText(String msg) { handle(msg); }
                    public void onClose(String reason) {
                        connecting = false;
                        listener.onStatus("disconnected");
                    }
                });
            } catch (Exception e) {
                connecting = false;
                ws = null;
                listener.onError("Can't reach Aria on " + host + ": " + e.getMessage());
            }
        }, "aria-ai").start();
    }

    private void trySend(JSONObject msg) {
        try { ws.sendText(msg.toString()); }
        catch (Exception e) { listener.onError("Send failed: " + e.getMessage()); }
    }

    private void handle(String raw) {
        JSONObject m;
        try { m = new JSONObject(raw); } catch (Exception e) { return; }

        // streamed events carry {event, params}; a "device" field means a per-device fleet worker
        String event = m.optString("event", "");
        if (!event.isEmpty()) {
            JSONObject p = m.optJSONObject("params");
            String device = p == null ? "" : p.optString("device", "");
            if ("ai.fleet.start".equals(event)) {
                listener.onFleetStart(device);
            } else if ("ai.fleet.done".equals(event)) {
                listener.onFleetDeviceDone(device, p != null && p.optBoolean("ok", false),
                        p == null ? "" : p.optString("text", ""), p == null ? "" : p.optString("error", ""));
            } else if ("ai.delta".equals(event)) {
                String text = p == null ? "" : p.optString("text", "");
                if (device.isEmpty()) listener.onDelta(text); else listener.onFleetDelta(device, text);
            } else if ("ai.tool".equals(event)) {
                String name = p == null ? "" : p.optString("name", "");
                if (device.isEmpty()) listener.onTool(name); else listener.onFleetTool(device, name);
            } else if ("ai.tool.result".equals(event)) {
                listener.onToolResult(device, p == null ? "" : p.optString("text", ""));
            } else if ("ai.approval".equals(event)) {
                String input = p == null || p.opt("input") == null ? "" : String.valueOf(p.opt("input"));
                listener.onApproval(p == null ? "" : p.optString("id", ""), p == null ? "" : p.optString("tool", ""), input);
            }
            return;
        }

        // a reply to one of our requests carries {id, result|error}
        if (m.has("id")) {
            long id = m.optLong("id", -1);
            JSONObject result = m.optJSONObject("result");
            if (result != null && result.has("session")) session = result.optString("session", session);
            if (m.has("error") && !m.isNull("error")) {
                listener.onError(m.optString("error"));
                return;
            }
            if (id == currentId && result != null && result.has("devices")) {
                listener.onFleetDone(result.optJSONArray("devices"));
            } else if (id == currentId) {
                listener.onDone(session, result == null ? "" : result.optString("text", ""));
            }
        }
    }

    private static String enc(String s) {
        try { return java.net.URLEncoder.encode(s, "UTF-8"); } catch (Exception e) { return s; }
    }
}

package com.bridgedesk.app;

import org.json.JSONObject;

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
        void onDelta(String text);          // a chunk of assistant text
        void onTool(String name);           // the assistant is running fleet tool <name>
        void onDone(String session, String text); // turn finished; session = resume id
        void onError(String message);
        void onStatus(String status);       // "connecting", "connected", "disconnected"
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
        final String path = "/ws?key=" + enc(store.key());
        final String fp = store.fingerprint();
        new Thread(() -> {
            try {
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

        // streamed events carry {event, params}
        String event = m.optString("event", "");
        if (!event.isEmpty()) {
            JSONObject p = m.optJSONObject("params");
            if ("ai.delta".equals(event)) {
                listener.onDelta(p == null ? "" : p.optString("text", ""));
            } else if ("ai.tool".equals(event)) {
                listener.onTool(p == null ? "" : p.optString("name", ""));
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
            if (id == currentId) {
                listener.onDone(session, result == null ? "" : result.optString("text", ""));
            }
        }
    }

    private static String enc(String s) {
        try { return java.net.URLEncoder.encode(s, "UTF-8"); } catch (Exception e) { return s; }
    }
}

package com.bridgedesk.app;

import android.content.Context;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.InputStream;
import java.io.OutputStream;
import java.net.URL;
import javax.net.ssl.HttpsURLConnection;

/**
 * Standalone on-device assistant: talks to the Anthropic Messages API directly (no PC), running
 * the agentic tool-use loop in the app with {@link StandaloneTools}. Reads run free; change tools
 * block on an Allow/Deny card via the Gate. The API key lives only in the Android Keystore.
 *
 * Non-streaming per round: each round returns text + any tool calls, which we render as activity;
 * the loop continues while Claude keeps calling tools, so the chat still shows live progress.
 */
public class Anthropic {
    private static final String URL = "https://api.anthropic.com/v1/messages";
    private static final String VERSION = "2023-06-01";
    private static final int MAX_ROUNDS = 12;

    public interface Listener {
        void onDelta(String text);
        void onTool(String name);
        void onToolResult(String text);
        void onDone(String text);
        void onError(String message);
    }
    /** Blocking Allow/Deny for a change tool. Returns true to allow. */
    public interface Gate { boolean allow(String tool, String inputJson); }

    private static final String PERSONA =
            "You are Aria, the user's personal assistant, running directly on their phone. You can act "
          + "with the phone's own tools (files, shell, clipboard, notifications, SMS/calls) and search "
          + "the web. Keep replies short and skimmable.\n"
          + "Safety: reading is free; anything that CHANGES things (write_file, run_sh, set_clipboard, "
          + "send_sms, dial) will ask the user to Allow or Deny — if denied, stop and say so. Never ask "
          + "for or enter passwords, PINs, or 2FA codes.";

    private final JSONArray messages = new JSONArray(); // conversation so far

    public synchronized void reset() { while (messages.length() > 0) messages.remove(0); }

    /** Run one user turn to completion on a background thread. */
    public void ask(final Context ctx, final Store st, final String prompt, final Listener cb, final Gate gate) {
        new Thread(() -> {
            try {
                String key = st.apiKey();
                if (key.isEmpty()) { cb.onError("No API key set — add your Anthropic key in Settings."); return; }
                synchronized (this) { messages.put(new JSONObject().put("role", "user").put("content", prompt)); }

                String finalText = "";
                for (int round = 0; round < MAX_ROUNDS; round++) {
                    JSONObject body = new JSONObject()
                            .put("model", st.model())
                            .put("max_tokens", 16000)
                            .put("system", PERSONA)
                            .put("messages", messages)
                            .put("tools", StandaloneTools.schemas());
                    JSONObject resp = post(key, body);
                    if (resp.has("__error")) { cb.onError(resp.optString("__error")); return; }

                    JSONArray content = resp.optJSONArray("content");
                    String stop = resp.optString("stop_reason", "");
                    if (content == null) { cb.onError("empty response"); return; }

                    // Render this round and collect any tool results.
                    JSONArray toolResults = new JSONArray();
                    for (int i = 0; i < content.length(); i++) {
                        JSONObject b = content.getJSONObject(i);
                        String type = b.optString("type");
                        if ("text".equals(type)) {
                            String t = b.optString("text");
                            if (!t.isEmpty()) { finalText = t; cb.onDelta(t); }
                        } else if ("tool_use".equals(type)) {
                            String name = b.optString("name");
                            JSONObject input = b.optJSONObject("input");
                            if (input == null) input = new JSONObject();
                            cb.onTool(name);
                            String out;
                            if (StandaloneTools.isChange(name) && !gate.allow(name, input.toString())) {
                                out = "Denied by the user.";
                            } else {
                                out = StandaloneTools.execute(ctx, st, name, input);
                            }
                            cb.onToolResult(snippet(out));
                            toolResults.put(new JSONObject()
                                    .put("type", "tool_result")
                                    .put("tool_use_id", b.optString("id"))
                                    .put("content", out));
                        } else if ("server_tool_use".equals(type)) {
                            cb.onTool(b.optString("name", "web_search"));
                        }
                    }

                    // Keep Claude's turn verbatim (tool_use + any thinking blocks) for the next round.
                    synchronized (this) { messages.put(new JSONObject().put("role", "assistant").put("content", content)); }

                    if ("tool_use".equals(stop)) {
                        synchronized (this) { messages.put(new JSONObject().put("role", "user").put("content", toolResults)); }
                        continue; // let Claude see the results
                    }
                    if ("pause_turn".equals(stop)) {
                        continue; // server tool (web search) still working — resend to continue
                    }
                    if ("refusal".equals(stop)) { cb.onDone(finalText.isEmpty() ? "(declined)" : finalText); return; }
                    cb.onDone(finalText); // end_turn / max_tokens
                    return;
                }
                cb.onDone(finalText.isEmpty() ? "(stopped after several steps)" : finalText);
            } catch (Exception e) {
                cb.onError(e.getMessage() == null ? e.toString() : e.getMessage());
            }
        }, "anthropic").start();
    }

    private JSONObject post(String key, JSONObject body) {
        try {
            HttpsURLConnection c = (HttpsURLConnection) new URL(URL).openConnection();
            c.setRequestMethod("POST");
            c.setDoOutput(true);
            c.setConnectTimeout(20000);
            c.setReadTimeout(180000);
            c.setRequestProperty("content-type", "application/json");
            c.setRequestProperty("x-api-key", key);
            c.setRequestProperty("anthropic-version", VERSION);
            try (OutputStream os = c.getOutputStream()) { os.write(body.toString().getBytes("UTF-8")); }
            int code = c.getResponseCode();
            InputStream in = code < 400 ? c.getInputStream() : c.getErrorStream();
            StringBuilder sb = new StringBuilder();
            if (in != null) { byte[] buf = new byte[8192]; int n; while ((n = in.read(buf)) > 0) sb.append(new String(buf, 0, n, "UTF-8")); }
            c.disconnect();
            if (code >= 400) {
                String msg = sb.toString();
                try { msg = new JSONObject(msg).getJSONObject("error").optString("message", msg); } catch (Exception ignored) {}
                return new JSONObject().put("__error", "Claude API " + code + ": " + msg);
            }
            return new JSONObject(sb.toString());
        } catch (Exception e) {
            try { return new JSONObject().put("__error", "network: " + e.getMessage()); } catch (Exception x) { return new JSONObject(); }
        }
    }

    private static String snippet(String s) {
        if (s == null) return "";
        s = s.trim().replace("\n", " ");
        return s.length() > 160 ? s.substring(0, 160) + "…" : s;
    }
}

package com.bridgedesk.app;

import android.content.Context;
import android.util.Base64;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.InputStream;
import java.net.URL;
import javax.net.ssl.HttpsURLConnection;

/**
 * The on-device tool surface for Standalone mode: the actions the phone's own Claude agent can
 * take without a PC, reusing the in-process device code ({@link Rpc}, {@link PhoneBridge},
 * {@link NotificationService}). Reads run free; change tools are gated by the agent loop before
 * execute() is called. web_search is a server-side Anthropic tool (no local execution).
 */
public class StandaloneTools {

    /** Tool definitions for the Messages API (custom tools + the web_search server tool). */
    static JSONArray schemas() {
        JSONArray a = new JSONArray();
        a.put(tool("device_info", "This phone's model, OS and id.", obj()));
        a.put(tool("list_files", "List a directory on the phone.", props("path", "string", "Absolute path; omit for internal storage root.", false)));
        a.put(tool("read_file", "Read a text file on the phone.", req1("path", "Absolute file path.")));
        a.put(tool("write_file", "Create/overwrite a text file on the phone. (change)", req2("path", "Absolute path.", "content", "File text.")));
        a.put(tool("run_sh", "Run one shell command on the phone and return stdout/stderr. (change)", req1("command", "Command line (sh).")));
        a.put(tool("get_clipboard", "Read the phone clipboard.", obj()));
        a.put(tool("set_clipboard", "Set the phone clipboard. (change)", req1("text", "Text to copy.")));
        a.put(tool("list_notifications", "Current phone notifications (needs notification access).", obj()));
        a.put(tool("sms_threads", "Recent SMS conversations.", props("limit", "integer", "Max threads (default 40).", false)));
        a.put(tool("send_sms", "Send an SMS. (change)", req2("to", "Phone number.", "text", "Message body.")));
        a.put(tool("dial", "Place a phone call. (change)", req1("number", "Phone number to dial.")));
        a.put(tool("call_log", "Recent calls.", props("limit", "integer", "Max entries (default 40).", false)));
        a.put(tool("http_get", "Fetch a URL (GET) and return the response body text.", req1("url", "https URL.")));
        // Server-side web search (Claude runs it; no local execution).
        JSONObject ws = new JSONObject();
        try { ws.put("type", "web_search_20260209").put("name", "web_search").put("max_uses", 5); } catch (Exception ignored) {}
        a.put(ws);
        return a;
    }

    static boolean isChange(String name) {
        switch (name) {
            case "write_file": case "run_sh": case "set_clipboard":
            case "send_sms": case "dial":
                return true;
            default:
                return false; // reads (and web_search / http_get) run free
        }
    }

    /** Execute a custom tool and return a text result. Reads are free; change gating is done upstream. */
    static String execute(Context ctx, Store st, String name, JSONObject in) {
        try {
            switch (name) {
                case "device_info": return Rpc.info(st).toString();
                case "list_files":  return Rpc.list(in.optString("path", "")).toString();
                case "read_file": {
                    JSONObject r = Rpc.read(in.optString("path"), 0, 1 << 20);
                    byte[] b = Base64.decode(r.optString("data", ""), Base64.DEFAULT);
                    return new String(b, "UTF-8");
                }
                case "write_file": {
                    String data = Base64.encodeToString(in.optString("content").getBytes("UTF-8"), Base64.NO_WRAP);
                    return Rpc.write(in.optString("path"), 0, data, true).toString();
                }
                case "run_sh":        return Rpc.exec(in.optString("command"), in.optInt("timeout", 60)).toString();
                case "get_clipboard": return Rpc.clipboardGet(ctx).toString();
                case "set_clipboard": Rpc.clipboardSet(ctx, in.optString("text")); return "ok";
                case "list_notifications":
                    return NotificationService.instance == null ? "notification access not granted on the phone"
                            : NotificationService.instance.listJson().toString();
                case "sms_threads":   return PhoneBridge.threads(ctx, in.optInt("limit", 40)).toString();
                case "send_sms":      return String.valueOf(PhoneBridge.sendSms(ctx, in.optString("to"), in.optString("text")));
                case "dial":          return String.valueOf(PhoneBridge.dial(ctx, in.optString("number")));
                case "call_log":      return PhoneBridge.callLog(ctx, in.optInt("limit", 40)).toString();
                case "http_get":      return httpGet(in.optString("url"));
                default:              return "unknown tool: " + name;
            }
        } catch (Exception e) {
            return "error: " + (e.getMessage() == null ? e.toString() : e.getMessage());
        }
    }

    private static String httpGet(String url) throws Exception {
        if (!url.startsWith("http")) url = "https://" + url;
        HttpsURLConnection c = (HttpsURLConnection) new URL(url).openConnection();
        c.setConnectTimeout(12000); c.setReadTimeout(20000);
        c.setRequestProperty("User-Agent", "Aria-Android");
        int code = c.getResponseCode();
        InputStream in = code < 400 ? c.getInputStream() : c.getErrorStream();
        StringBuilder sb = new StringBuilder();
        if (in != null) { byte[] buf = new byte[8192]; int n; while ((n = in.read(buf)) > 0 && sb.length() < 100_000) sb.append(new String(buf, 0, n, "UTF-8")); }
        c.disconnect();
        return "HTTP " + code + "\n" + sb;
    }

    // ---- schema helpers ----
    private static JSONObject tool(String name, String desc, JSONObject schema) {
        JSONObject t = new JSONObject();
        try { t.put("name", name).put("description", desc).put("input_schema", schema); } catch (Exception ignored) {}
        return t;
    }
    private static JSONObject obj() {
        JSONObject s = new JSONObject();
        try { s.put("type", "object").put("properties", new JSONObject()); } catch (Exception ignored) {}
        return s;
    }
    private static JSONObject props(String k, String type, String desc, boolean required) {
        JSONObject s = obj();
        try {
            JSONObject p = new JSONObject();
            p.put(k, new JSONObject().put("type", type).put("description", desc));
            s.put("properties", p);
            if (required) s.put("required", new JSONArray().put(k));
        } catch (Exception ignored) {}
        return s;
    }
    private static JSONObject req1(String k, String desc) { return props(k, "string", desc, true); }
    private static JSONObject req2(String k1, String d1, String k2, String d2) {
        JSONObject s = obj();
        try {
            JSONObject p = new JSONObject();
            p.put(k1, new JSONObject().put("type", "string").put("description", d1));
            p.put(k2, new JSONObject().put("type", "string").put("description", d2));
            s.put("properties", p).put("required", new JSONArray().put(k1).put(k2));
        } catch (Exception ignored) {}
        return s;
    }
}

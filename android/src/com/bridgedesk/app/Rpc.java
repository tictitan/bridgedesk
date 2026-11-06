package com.bridgedesk.app;

import android.content.ClipData;
import android.content.ClipboardManager;
import android.content.Context;
import android.os.Build;
import android.os.Environment;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.*;
import java.util.*;

/** Phone-side implementation of the BridgeDesk device RPC surface. */
public class Rpc {

    static Object call(AgentService svc, String method, JSONObject p) throws Exception {
        Store st = svc.store();
        switch (method) {
            case "info": return info(st);
            case "exec": {
                if (!st.allowShell()) throw new Exception("shell disabled on phone");
                return exec(p.optString("command"), p.optInt("timeout", 60));
            }
            case "fs.roots": {
                if (!st.allowFiles()) throw new Exception("file access disabled on phone");
                return roots();
            }
            case "fs.list": {
                if (!st.allowFiles()) throw new Exception("file access disabled on phone");
                return list(p.optString("path"));
            }
            case "fs.read": return read(p.optString("path"), p.optLong("offset", 0), p.optInt("length", 1 << 20));
            case "fs.write": return write(p.optString("path"), p.optLong("offset", 0), p.optString("data"), p.optBoolean("truncate", false));
            case "fs.mkdir": { new File(p.optString("path")).mkdirs(); return Boolean.TRUE; }
            case "fs.delete": return delete(new File(p.optString("path")), p.optBoolean("recursive", false));
            case "fs.rename": return new File(p.optString("from")).renameTo(new File(p.optString("to")));
            case "screen.info": return screenInfo(svc);
            case "screen.capture": {
                if (!st.allowControl()) throw new Exception("screen sharing disabled on phone");
                return screenCapture(svc, p);
            }
            case "input.mouse": {
                if (!st.allowControl()) throw new Exception("remote control disabled on phone");
                return inputMouse(svc, p);
            }
            case "input.key": return inputKey(p.optString("key"));
            case "input.text": return inputText(p.optString("text"));
            case "clipboard.get": return clipboardGet(svc);
            case "clipboard.set": return clipboardSet(svc, p.optString("text"));
            case "notif.permission": {
                JSONObject o = new JSONObject();
                o.put("enabled", NotificationService.isEnabled(svc));
                o.put("allowed", st.allowNotifications());
                return o;
            }
            case "notif.list": {
                if (!st.allowNotifications()) throw new Exception("notification mirroring disabled on phone");
                NotificationService ns = NotificationService.instance;
                if (ns == null) throw new Exception("grant notification access on the phone (Settings → Notification access → BridgeDesk)");
                JSONObject o = new JSONObject();
                o.put("notifications", ns.listJson());
                return o;
            }
            case "notif.action": {
                NotificationService ns = NotificationService.instance;
                if (ns == null) throw new Exception("notification access not granted on phone");
                return ns.act(p.optString("key"), p.optInt("action", -1));
            }
            case "notif.open": {
                NotificationService ns = NotificationService.instance;
                if (ns == null) throw new Exception("notification access not granted on phone");
                return ns.act(p.optString("key"), -1); // fire the notification's content intent (open the app)
            }
            case "notif.reply": {
                NotificationService ns = NotificationService.instance;
                if (ns == null) throw new Exception("notification access not granted on phone");
                return ns.reply(p.optString("key"), p.optInt("action", 0), p.optString("text"));
            }
            case "notif.dismiss": {
                NotificationService ns = NotificationService.instance;
                if (ns == null) throw new Exception("notification access not granted on phone");
                return ns.dismiss(p.optString("key"));
            }
            case "sys.open": return Boolean.TRUE;
        }
        throw new Exception("unknown method: " + method);
    }

    static JSONObject info(Store st) throws Exception {
        JSONObject o = new JSONObject();
        o.put("id", st.deviceId());
        o.put("name", st.deviceName());
        o.put("kind", "android");
        o.put("os", "Android " + Build.VERSION.RELEASE);
        o.put("platform", Build.MANUFACTURER + " " + Build.MODEL);
        return o;
    }

    static JSONObject exec(String command, int timeout) throws Exception {
        long start = System.currentTimeMillis();
        ProcessBuilder pb = new ProcessBuilder("sh", "-c", command);
        pb.redirectErrorStream(false);
        Process proc = pb.start();
        StringBuilder out = new StringBuilder(), err = new StringBuilder();
        Thread to = drain(proc.getInputStream(), out);
        Thread te = drain(proc.getErrorStream(), err);
        boolean done = proc.waitFor(Math.max(1, timeout), java.util.concurrent.TimeUnit.SECONDS);
        JSONObject o = new JSONObject();
        if (!done) {
            proc.destroyForcibly();
            o.put("timedOut", true);
            o.put("exitCode", -1);
        } else {
            o.put("exitCode", proc.exitValue());
        }
        to.join(2000); te.join(2000);
        o.put("stdout", out.toString());
        o.put("stderr", err.toString());
        o.put("durationMs", System.currentTimeMillis() - start);
        return o;
    }

    private static Thread drain(InputStream in, StringBuilder sb) {
        Thread t = new Thread(() -> {
            try {
                byte[] b = new byte[8192]; int n;
                while ((n = in.read(b)) > 0 && sb.length() < 2_000_000) sb.append(new String(b, 0, n, "UTF-8"));
            } catch (Exception ignored) {}
        });
        t.start();
        return t;
    }

    static JSONArray roots() throws Exception {
        JSONArray a = new JSONArray();
        a.put(root("Internal storage", Environment.getExternalStorageDirectory().getAbsolutePath()));
        for (String s : new String[]{"Download", "DCIM", "Pictures", "Documents", "Movies", "Music"}) {
            File f = new File(Environment.getExternalStorageDirectory(), s);
            if (f.isDirectory()) a.put(root(s, f.getAbsolutePath()));
        }
        File[] ext = null;
        try { ext = new File("/storage").listFiles(); } catch (Exception ignored) {}
        if (ext != null) for (File f : ext) {
            String n = f.getName();
            if (!n.equals("emulated") && !n.equals("self") && f.canRead()) a.put(root("SD/" + n, f.getAbsolutePath()));
        }
        return a;
    }
    private static JSONObject root(String name, String path) throws Exception {
        JSONObject o = new JSONObject(); o.put("name", name); o.put("path", path); return o;
    }

    static JSONArray list(String path) throws Exception {
        if (path == null || path.isEmpty()) path = Environment.getExternalStorageDirectory().getAbsolutePath();
        File dir = new File(path);
        File[] files = dir.listFiles();
        if (files == null) throw new Exception("cannot read " + path);
        ArrayList<File> fs = new ArrayList<>(Arrays.asList(files));
        Collections.sort(fs, (a, b) -> {
            if (a.isDirectory() != b.isDirectory()) return a.isDirectory() ? -1 : 1;
            return a.getName().compareToIgnoreCase(b.getName());
        });
        JSONArray a = new JSONArray();
        for (File f : fs) {
            JSONObject o = new JSONObject();
            o.put("name", f.getName());
            o.put("path", f.getAbsolutePath());
            o.put("dir", f.isDirectory());
            o.put("size", f.length());
            o.put("mtime", f.lastModified() / 1000);
            a.put(o);
        }
        return a;
    }

    static JSONObject read(String path, long offset, int length) throws Exception {
        if (length <= 0 || length > 4 << 20) length = 1 << 20;
        File f = new File(path);
        RandomAccessFile raf = new RandomAccessFile(f, "r");
        try {
            raf.seek(offset);
            byte[] buf = new byte[length];
            int n = 0, r;
            while (n < length && (r = raf.read(buf, n, length - n)) > 0) n += r;
            JSONObject o = new JSONObject();
            o.put("data", AgentService.b64(Arrays.copyOf(buf, Math.max(0, n))));
            o.put("eof", offset + n >= f.length());
            o.put("size", f.length());
            return o;
        } finally { raf.close(); }
    }

    static JSONObject write(String path, long offset, String dataB64, boolean truncate) throws Exception {
        byte[] b = AgentService.unb64(dataB64);
        File f = new File(path);
        if (f.getParentFile() != null) f.getParentFile().mkdirs();
        RandomAccessFile raf = new RandomAccessFile(f, "rw");
        try {
            if (truncate && offset == 0) raf.setLength(0);
            raf.seek(offset);
            raf.write(b);
        } finally { raf.close(); }
        JSONObject o = new JSONObject(); o.put("written", b.length); return o;
    }

    static boolean delete(File f, boolean recursive) {
        if (recursive && f.isDirectory()) {
            File[] c = f.listFiles();
            if (c != null) for (File x : c) delete(x, true);
        }
        return f.delete();
    }

    static JSONObject screenInfo(AgentService svc) throws Exception {
        JSONObject o = new JSONObject();
        o.put("width", svc.capture().realWidth());
        o.put("height", svc.capture().realHeight());
        return o;
    }

    static JSONObject screenCapture(AgentService svc, JSONObject p) throws Exception {
        if (!ControlService.available() && false) {}
        svc.capture().configure(p.optInt("maxWidth", 1280), p.optInt("quality", 60));
        svc.ensureProjection();
        // wait briefly for the first frame after a cold start
        byte[] jpg = svc.capture().latestJpeg();
        for (int i = 0; i < 40 && jpg == null; i++) { Thread.sleep(100); jpg = svc.capture().latestJpeg(); }
        if (jpg == null) throw new Exception("screen not available yet — grant the screen-capture prompt on the phone");
        JSONObject o = new JSONObject();
        o.put("data", AgentService.b64(jpg));
        o.put("mime", "image/jpeg");
        o.put("w", svc.capture().frameWidth());
        o.put("h", svc.capture().frameHeight());
        o.put("width", svc.capture().realWidth());
        o.put("height", svc.capture().realHeight());
        return o;
    }

    static Object inputMouse(AgentService svc, JSONObject p) throws Exception {
        ControlService cs = ControlService.instance;
        if (cs == null) throw new Exception("enable the BridgeDesk accessibility service on the phone to allow remote control");
        int w = svc.capture().realWidth(), h = svc.capture().realHeight();
        if (w == 0) { w = 1080; h = 1920; }
        float x = (float) p.optDouble("x", 0) * w;
        float y = (float) p.optDouble("y", 0) * h;
        String action = p.optString("action");
        switch (action) {
            case "click": case "tap": cs.tap(x, y); break;
            case "down": case "up": break; // ignore raw button halves; a tap is sent on "click"
            case "dblclick": cs.tap(x, y); Thread.sleep(90); cs.tap(x, y); break;
            case "longpress": cs.longPress(x, y); break;
            case "swipe": {
                float x2 = (float) p.optDouble("x2", 0) * w;
                float y2 = (float) p.optDouble("y2", 0) * h;
                int ms = p.optInt("ms", 220);
                cs.swipe(x, y, x2, y2, ms);
                break;
            }
            case "scroll": {
                int dy = p.optInt("dy", 3);
                float cx = w / 2f, cy = h / 2f;
                float d = Math.signum(dy) * h * 0.22f;
                cs.swipe(cx, cy, cx, cy + d, 160); // dy>0 (wheel down) scrolls content up
                break;
            }
            case "move": break; // no cursor on Android
        }
        return Boolean.TRUE;
    }

    static Object inputKey(String key) throws Exception {
        ControlService cs = ControlService.instance;
        if (cs == null) throw new Exception("accessibility service not enabled on phone");
        switch (key) {
            case "enter": return cs.typeText("\n");
            case "tab": return cs.typeText("\t");
            case "del": case "backspace": return cs.deleteChar();
        }
        if (cs.globalKey(key)) return Boolean.TRUE;
        return cs.typeText(key);
    }

    static Object inputText(String text) throws Exception {
        ControlService cs = ControlService.instance;
        if (cs == null) throw new Exception("accessibility service not enabled on phone");
        if (!cs.typeText(text)) throw new Exception("no focused text field to type into");
        return Boolean.TRUE;
    }

    static JSONObject clipboardGet(Context c) throws Exception {
        final JSONObject o = new JSONObject();
        runOnMain(c, () -> {
            try {
                ClipboardManager cm = (ClipboardManager) c.getSystemService(Context.CLIPBOARD_SERVICE);
                CharSequence t = "";
                if (cm != null && cm.hasPrimaryClip() && cm.getPrimaryClip().getItemCount() > 0)
                    t = cm.getPrimaryClip().getItemAt(0).coerceToText(c);
                o.put("text", t == null ? "" : t.toString());
            } catch (Exception e) { try { o.put("text", ""); } catch (Exception ignored) {} }
        });
        return o;
    }

    static Object clipboardSet(Context c, String text) throws Exception {
        runOnMain(c, () -> {
            ClipboardManager cm = (ClipboardManager) c.getSystemService(Context.CLIPBOARD_SERVICE);
            if (cm != null) cm.setPrimaryClip(ClipData.newPlainText("BridgeDesk", text));
        });
        return Boolean.TRUE;
    }

    private static void runOnMain(Context c, Runnable r) {
        final Object lock = new Object();
        final boolean[] done = {false};
        new android.os.Handler(android.os.Looper.getMainLooper()).post(() -> {
            try { r.run(); } finally { synchronized (lock) { done[0] = true; lock.notifyAll(); } }
        });
        synchronized (lock) { long t = System.currentTimeMillis(); while (!done[0] && System.currentTimeMillis() - t < 2000) try { lock.wait(200); } catch (InterruptedException ignored) {} }
    }
}

package com.bridgedesk.app;

import android.content.Context;
import android.content.SharedPreferences;
import android.os.Build;

import java.security.SecureRandom;

/** Persistent connection settings and this device's identity. */
public class Store {
    private final SharedPreferences sp;

    public Store(Context c) { sp = c.getSharedPreferences("bridgedesk", Context.MODE_PRIVATE); }

    public String host() { return sp.getString("host", ""); }
    public int port() { return sp.getInt("port", 47810); }
    public String key() { return sp.getString("key", ""); }
    public String fingerprint() { return sp.getString("fp", ""); }
    public boolean agentEnabled() { return sp.getBoolean("agent", false); }
    public boolean allowControl() { return sp.getBoolean("allowControl", true); }
    public boolean allowFiles() { return sp.getBoolean("allowFiles", true); }
    public boolean allowShell() { return sp.getBoolean("allowShell", true); }
    public boolean allowNotifications() { return sp.getBoolean("allowNotifications", true); }

    public void setConnection(String host, int port, String key, String fp) {
        sp.edit().putString("host", host).putString("key", key).putInt("port", port).putString("fp", fp).apply();
    }
    public void setAgentEnabled(boolean v) { sp.edit().putBoolean("agent", v).apply(); }
    public void setAllow(String which, boolean v) { sp.edit().putBoolean(which, v).apply(); }

    public String deviceId() {
        String id = sp.getString("deviceId", null);
        if (id == null) {
            byte[] b = new byte[6];
            new SecureRandom().nextBytes(b);
            StringBuilder sb = new StringBuilder("and-");
            for (byte x : b) sb.append(String.format("%02x", x));
            id = sb.toString();
            sp.edit().putString("deviceId", id).apply();
        }
        return id;
    }

    public String deviceName() {
        String n = sp.getString("name", null);
        if (n == null || n.isEmpty()) {
            n = Build.MODEL != null ? Build.MODEL : "Android phone";
        }
        return n;
    }
    public void setDeviceName(String n) { sp.edit().putString("name", n).apply(); }
}

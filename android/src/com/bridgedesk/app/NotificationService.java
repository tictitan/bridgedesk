package com.bridgedesk.app;

import android.app.Notification;
import android.app.PendingIntent;
import android.app.RemoteInput;
import android.content.ComponentName;
import android.content.Context;
import android.content.Intent;
import android.content.pm.ApplicationInfo;
import android.content.pm.PackageManager;
import android.os.Bundle;
import android.provider.Settings;
import android.service.notification.NotificationListenerService;
import android.service.notification.StatusBarNotification;

import org.json.JSONArray;
import org.json.JSONObject;

/**
 * Mirrors this phone's notifications to the PC: messages (SMS/chat), and push prompts such as
 * Okta Verify. The PC can list active notifications, remotely tap an action (e.g. "Approve"),
 * or dismiss one. Requires the user to grant Notification access in Android settings.
 */
public class NotificationService extends NotificationListenerService {
    static NotificationService instance;

    @Override public void onListenerConnected() { instance = this; }
    @Override public void onListenerDisconnected() { instance = null; }

    @Override public void onNotificationPosted(StatusBarNotification sbn) {
        AgentService a = AgentService.instance;
        if (a == null || !a.store().allowNotifications()) return;
        if (isNoise(sbn, this)) return;
        try { a.emit("notif.posted", toJson(sbn, this)); } catch (Exception ignored) {}
    }

    /** Skip what nobody wants on their PC: our own service notice, group summaries, silent ongoing ones (music, USB...). */
    static boolean isNoise(StatusBarNotification sbn, Context ctx) {
        if (ctx.getPackageName().equals(sbn.getPackageName())) return true;
        Notification n = sbn.getNotification();
        if ((n.flags & Notification.FLAG_GROUP_SUMMARY) != 0) return true;
        boolean ongoing = (n.flags & Notification.FLAG_ONGOING_EVENT) != 0;
        if (ongoing && !Notification.CATEGORY_CALL.equals(n.category)) return true;
        Bundle ex = n.extras;
        return ex == null || (str(ex, Notification.EXTRA_TITLE).isEmpty() && str(ex, Notification.EXTRA_TEXT).isEmpty());
    }

    @Override public void onNotificationRemoved(StatusBarNotification sbn) {
        AgentService a = AgentService.instance;
        if (a == null) return;
        try {
            JSONObject o = new JSONObject();
            o.put("sid", "notif");
            o.put("key", sbn.getKey());
            a.emit("notif.removed", o);
        } catch (Exception ignored) {}
    }

    /** Whether the user has granted notification access to this app. */
    static boolean isEnabled(Context c) {
        String flat = Settings.Secure.getString(c.getContentResolver(), "enabled_notification_listeners");
        return flat != null && flat.contains(c.getPackageName());
    }

    JSONArray listJson() throws Exception {
        JSONArray arr = new JSONArray();
        StatusBarNotification[] active;
        try { active = getActiveNotifications(); } catch (Exception e) { return arr; }
        if (active == null) return arr;
        for (StatusBarNotification sbn : active) {
            JSONObject o = toJson(sbn, this);
            o.remove("sid");
            arr.put(o);
        }
        return arr;
    }

    /** Trigger the Nth action of a notification (e.g. Okta "Approve"); -1 taps the notification body. */
    boolean act(String key, int actionIndex) {
        StatusBarNotification sbn = find(key);
        if (sbn == null) return false;
        Notification n = sbn.getNotification();
        try {
            if (actionIndex < 0) {
                if (n.contentIntent != null) { n.contentIntent.send(); return true; }
                return false;
            }
            if (n.actions != null && actionIndex < n.actions.length) {
                PendingIntent pi = n.actions[actionIndex].actionIntent;
                if (pi != null) { pi.send(); return true; }
            }
        } catch (PendingIntent.CanceledException e) { return false; }
        return false;
    }

    /** Send an inline reply: fill the action's RemoteInput(s) with text and fire it (e.g. WhatsApp Reply). */
    boolean reply(String key, int actionIndex, String text) {
        StatusBarNotification sbn = find(key);
        if (sbn == null) return false;
        Notification n = sbn.getNotification();
        if (n.actions == null || actionIndex < 0 || actionIndex >= n.actions.length) return false;
        Notification.Action a = n.actions[actionIndex];
        RemoteInput[] ris = a.getRemoteInputs();
        if (ris == null || ris.length == 0) {
            return act(key, actionIndex); // not a text reply — just trigger it
        }
        Intent intent = new Intent();
        Bundle results = new Bundle();
        for (RemoteInput ri : ris) results.putCharSequence(ri.getResultKey(), text);
        RemoteInput.addResultsToIntent(ris, intent, results);
        try {
            a.actionIntent.send(this, 0, intent);
            return true;
        } catch (PendingIntent.CanceledException e) {
            return false;
        }
    }

    boolean dismiss(String key) {
        try { cancelNotification(key); return true; } catch (Exception e) { return false; }
    }

    private StatusBarNotification find(String key) {
        try {
            for (StatusBarNotification s : getActiveNotifications())
                if (s.getKey().equals(key)) return s;
        } catch (Exception ignored) {}
        return null;
    }

    static JSONObject toJson(StatusBarNotification sbn, Context ctx) throws Exception {
        Notification n = sbn.getNotification();
        Bundle ex = n.extras;
        JSONObject o = new JSONObject();
        o.put("sid", "notif");
        o.put("key", sbn.getKey());
        o.put("pkg", sbn.getPackageName());
        o.put("app", appLabel(ctx, sbn.getPackageName()));
        o.put("time", sbn.getPostTime());
        String title = str(ex, Notification.EXTRA_TITLE_BIG);
        if (title.isEmpty()) title = str(ex, Notification.EXTRA_TITLE);
        o.put("title", title);
        o.put("text", fullText(ex));
        o.put("category", n.category == null ? "" : n.category);
        o.put("sub", str(ex, Notification.EXTRA_SUB_TEXT));
        o.put("clearable", sbn.isClearable());
        o.put("ongoing", (n.flags & Notification.FLAG_ONGOING_EVENT) != 0);
        JSONArray acts = new JSONArray();
        if (n.actions != null) for (Notification.Action a : n.actions) {
            JSONObject ao = new JSONObject();
            ao.put("title", a.title == null ? "" : a.title.toString());
            RemoteInput[] ris = a.getRemoteInputs();
            ao.put("reply", ris != null && ris.length > 0);
            acts.put(ao);
        }
        o.put("actions", acts);
        o.put("canOpen", n.contentIntent != null);
        return o;
    }

    /** The most complete text: expanded text, inbox lines, or the recent messages of a chat. */
    static String fullText(Bundle ex) {
        String text = str(ex, Notification.EXTRA_TEXT);
        String big = str(ex, Notification.EXTRA_BIG_TEXT);
        if (big.length() > text.length()) text = big;
        try {
            android.os.Parcelable[] msgs = ex.getParcelableArray(Notification.EXTRA_MESSAGES);
            if (msgs != null && msgs.length > 1) {
                StringBuilder sb = new StringBuilder();
                for (int i = Math.max(0, msgs.length - 5); i < msgs.length; i++) {
                    if (!(msgs[i] instanceof Bundle)) continue;
                    Bundle m = (Bundle) msgs[i];
                    CharSequence who = m.getCharSequence("sender");
                    CharSequence body = m.getCharSequence("text");
                    if (body == null) continue;
                    if (sb.length() > 0) sb.append('\n');
                    if (who != null && who.length() > 0) sb.append(who).append(": ");
                    sb.append(body);
                }
                if (sb.length() > text.length()) text = sb.toString();
            }
            CharSequence[] lines = ex.getCharSequenceArray(Notification.EXTRA_TEXT_LINES);
            if (lines != null && lines.length > 0) {
                StringBuilder sb = new StringBuilder();
                for (CharSequence l : lines) { if (l == null) continue; if (sb.length() > 0) sb.append('\n'); sb.append(l); }
                if (sb.length() > text.length()) text = sb.toString();
            }
        } catch (Exception ignored) {}
        return text;
    }

    private static String str(Bundle b, String key) {
        CharSequence c = b.getCharSequence(key);
        return c == null ? "" : c.toString();
    }

    private static String appLabel(Context ctx, String pkg) {
        try {
            PackageManager pm = ctx.getPackageManager();
            ApplicationInfo ai = pm.getApplicationInfo(pkg, 0);
            return pm.getApplicationLabel(ai).toString();
        } catch (Exception e) { return pkg; }
    }
}

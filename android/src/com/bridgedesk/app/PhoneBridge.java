package com.bridgedesk.app;

import android.Manifest;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.pm.PackageManager;
import android.database.Cursor;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.provider.CallLog;
import android.provider.ContactsContract;
import android.provider.Telephony;
import android.telecom.TelecomManager;
import android.telephony.SmsManager;
import android.telephony.SmsMessage;
import android.telephony.TelephonyManager;

import org.json.JSONArray;
import org.json.JSONObject;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.Map;

/**
 * Calls and texts for the PC: forwards ringing / missed calls and incoming SMS as events, and lets
 * the PC read SMS conversations, send SMS, answer / end / place calls, read the call log, and
 * send a WhatsApp message (opened in WhatsApp and sent through the accessibility service).
 * Call audio stays on the phone (or its headset) — Android does not let apps route it elsewhere.
 */
@SuppressWarnings("deprecation")
public class PhoneBridge {
    static final String[] PERMS = {
            Manifest.permission.READ_SMS, Manifest.permission.SEND_SMS, Manifest.permission.RECEIVE_SMS,
            Manifest.permission.READ_CALL_LOG, Manifest.permission.READ_PHONE_STATE, Manifest.permission.CALL_PHONE,
            Manifest.permission.ANSWER_PHONE_CALLS, Manifest.permission.READ_CONTACTS,
    };

    private final AgentService svc;
    private BroadcastReceiver receiver;
    private String lastState = TelephonyManager.EXTRA_STATE_IDLE;
    private String lastNumber = "";
    private static final Map<String, String> nameCache = new HashMap<>();

    PhoneBridge(AgentService svc) { this.svc = svc; }

    static boolean has(Context c, String perm) {
        return c.checkSelfPermission(perm) == PackageManager.PERMISSION_GRANTED;
    }

    static boolean allGranted(Context c) {
        for (String p : PERMS) if (!has(c, p)) return false;
        return true;
    }

    static boolean anyGranted(Context c) {
        return has(c, Manifest.permission.READ_SMS) || has(c, Manifest.permission.READ_CALL_LOG);
    }

    private static void need(Context c, String perm) throws Exception {
        if (!has(c, perm)) {
            throw new Exception("the phone needs the " + perm.substring(perm.lastIndexOf('.') + 1)
                    + " permission — open BridgeDesk on the phone → Phone access → Calls & SMS");
        }
    }

    static JSONObject status(Context c) throws Exception {
        JSONObject o = new JSONObject();
        for (String p : PERMS) o.put(p.substring(p.lastIndexOf('.') + 1), has(c, p));
        o.put("whatsapp", installed(c, "com.whatsapp"));
        o.put("whatsappBusiness", installed(c, "com.whatsapp.w4b"));
        o.put("canAutoSend", ControlService.available());
        return o;
    }

    private static boolean installed(Context c, String pkg) {
        try { c.getPackageManager().getPackageInfo(pkg, 0); return true; } catch (Exception e) { return false; }
    }

    // ---------------- live events ----------------

    void start() {
        if (receiver != null) return;
        receiver = new BroadcastReceiver() {
            @Override public void onReceive(Context ctx, Intent i) {
                try {
                    if (!svc.store().allowPhone()) return;
                    String a = i.getAction();
                    if (TelephonyManager.ACTION_PHONE_STATE_CHANGED.equals(a)) onPhoneState(i);
                    else if (Telephony.Sms.Intents.SMS_RECEIVED_ACTION.equals(a)) onSms(i);
                } catch (Exception ignored) {}
            }
        };
        IntentFilter f = new IntentFilter();
        f.addAction(TelephonyManager.ACTION_PHONE_STATE_CHANGED);
        f.addAction(Telephony.Sms.Intents.SMS_RECEIVED_ACTION);
        try {
            if (Build.VERSION.SDK_INT >= 33) svc.registerReceiver(receiver, f, Context.RECEIVER_EXPORTED);
            else svc.registerReceiver(receiver, f);
        } catch (Exception e) { receiver = null; }
    }

    void stop() {
        if (receiver == null) return;
        try { svc.unregisterReceiver(receiver); } catch (Exception ignored) {}
        receiver = null;
    }

    private void onPhoneState(Intent i) throws Exception {
        String state = i.getStringExtra(TelephonyManager.EXTRA_STATE);
        if (state == null) return;
        String num = i.getStringExtra(TelephonyManager.EXTRA_INCOMING_NUMBER);
        boolean changed = !state.equals(lastState);
        if (num != null && !num.isEmpty() && !num.equals(lastNumber)) { lastNumber = num; changed = true; }
        if (!changed) return;
        String prev = lastState;
        lastState = state;
        String s;
        if (TelephonyManager.EXTRA_STATE_RINGING.equals(state)) s = "ringing";
        else if (TelephonyManager.EXTRA_STATE_OFFHOOK.equals(state)) s = "active";
        else s = "ended";
        JSONObject o = new JSONObject();
        o.put("sid", "phone");
        o.put("state", s);
        o.put("number", lastNumber);
        o.put("name", contactName(svc, lastNumber));
        o.put("missed", "ended".equals(s) && TelephonyManager.EXTRA_STATE_RINGING.equals(prev));
        o.put("time", System.currentTimeMillis());
        svc.emit("call.state", o);
        if ("ended".equals(s)) lastNumber = "";
    }

    private void onSms(Intent i) throws Exception {
        SmsMessage[] parts = Telephony.Sms.Intents.getMessagesFromIntent(i);
        if (parts == null || parts.length == 0 || parts[0] == null) return;
        StringBuilder body = new StringBuilder();
        for (SmsMessage m : parts) if (m != null && m.getMessageBody() != null) body.append(m.getMessageBody());
        String from = parts[0].getOriginatingAddress();
        JSONObject o = new JSONObject();
        o.put("sid", "phone");
        o.put("from", from == null ? "" : from);
        o.put("name", contactName(svc, from));
        o.put("text", body.toString());
        o.put("time", System.currentTimeMillis());
        svc.emit("sms.received", o);
    }

    static String contactName(Context c, String number) {
        if (number == null || number.isEmpty() || !has(c, Manifest.permission.READ_CONTACTS)) return "";
        synchronized (nameCache) {
            String hit = nameCache.get(number);
            if (hit != null) return hit;
        }
        String name = "";
        Uri u = Uri.withAppendedPath(ContactsContract.PhoneLookup.CONTENT_FILTER_URI, Uri.encode(number));
        try (Cursor cur = c.getContentResolver().query(u, new String[]{ContactsContract.PhoneLookup.DISPLAY_NAME}, null, null, null)) {
            if (cur != null && cur.moveToFirst()) name = cur.getString(0);
        } catch (Exception ignored) {}
        if (name == null) name = "";
        synchronized (nameCache) { nameCache.put(number, name); }
        return name;
    }

    // ---------------- SMS ----------------

    static JSONObject threads(Context c, int limit) throws Exception {
        need(c, Manifest.permission.READ_SMS);
        if (limit <= 0) limit = 60;
        Map<Long, JSONObject> out = new LinkedHashMap<>();
        Map<Long, Integer> unread = new HashMap<>();
        String[] cols = {Telephony.Sms.THREAD_ID, Telephony.Sms.ADDRESS, Telephony.Sms.BODY, Telephony.Sms.DATE, Telephony.Sms.TYPE, Telephony.Sms.READ};
        try (Cursor cur = c.getContentResolver().query(Telephony.Sms.CONTENT_URI, cols, null, null, "date DESC")) {
            int scanned = 0;
            while (cur != null && cur.moveToNext() && scanned++ < 6000) {
                long tid = cur.getLong(0);
                int type = cur.getInt(4);
                if (cur.getInt(5) == 0 && type == Telephony.Sms.MESSAGE_TYPE_INBOX) {
                    Integer n = unread.get(tid);
                    unread.put(tid, n == null ? 1 : n + 1);
                }
                if (out.containsKey(tid) || out.size() >= limit) continue;
                String addr = cur.getString(1);
                if (addr == null) addr = "";
                JSONObject t = new JSONObject();
                t.put("thread", tid);
                t.put("address", addr);
                t.put("name", contactName(c, addr));
                String body = cur.getString(2);
                t.put("snippet", body == null ? "" : body);
                t.put("time", cur.getLong(3));
                t.put("outgoing", type != Telephony.Sms.MESSAGE_TYPE_INBOX);
                out.put(tid, t);
            }
        }
        JSONArray arr = new JSONArray();
        for (Map.Entry<Long, JSONObject> e : out.entrySet()) {
            Integer n = unread.get(e.getKey());
            e.getValue().put("unread", n == null ? 0 : n);
            arr.put(e.getValue());
        }
        JSONObject o = new JSONObject();
        o.put("threads", arr);
        return o;
    }

    static JSONObject messages(Context c, long thread, String address, int limit) throws Exception {
        need(c, Manifest.permission.READ_SMS);
        if (limit <= 0) limit = 80;
        String sel;
        String[] args;
        if (thread > 0) { sel = Telephony.Sms.THREAD_ID + "=?"; args = new String[]{String.valueOf(thread)}; }
        else { sel = Telephony.Sms.ADDRESS + "=?"; args = new String[]{address == null ? "" : address}; }
        String[] cols = {Telephony.Sms._ID, Telephony.Sms.ADDRESS, Telephony.Sms.BODY, Telephony.Sms.DATE, Telephony.Sms.TYPE};
        ArrayList<JSONObject> rows = new ArrayList<>();
        try (Cursor cur = c.getContentResolver().query(Telephony.Sms.CONTENT_URI, cols, sel, args, "date DESC")) {
            while (cur != null && cur.moveToNext() && rows.size() < limit) {
                JSONObject m = new JSONObject();
                m.put("id", cur.getLong(0));
                String addr = cur.getString(1);
                m.put("address", addr == null ? "" : addr);
                String body = cur.getString(2);
                m.put("text", body == null ? "" : body);
                m.put("time", cur.getLong(3));
                int type = cur.getInt(4);
                m.put("outgoing", type != Telephony.Sms.MESSAGE_TYPE_INBOX);
                m.put("status", type == Telephony.Sms.MESSAGE_TYPE_FAILED ? "failed"
                        : (type == Telephony.Sms.MESSAGE_TYPE_OUTBOX || type == Telephony.Sms.MESSAGE_TYPE_QUEUED) ? "sending" : "");
                rows.add(m);
            }
        }
        JSONArray arr = new JSONArray();
        for (int k = rows.size() - 1; k >= 0; k--) arr.put(rows.get(k)); // oldest first
        JSONObject o = new JSONObject();
        o.put("messages", arr);
        String who = thread > 0 && !rows.isEmpty() ? rows.get(0).optString("address") : address;
        o.put("address", who == null ? "" : who);
        o.put("name", contactName(c, who));
        return o;
    }

    static Object sendSms(Context c, String to, String text) throws Exception {
        need(c, Manifest.permission.SEND_SMS);
        if (to == null || to.trim().isEmpty()) throw new Exception("no recipient");
        if (text == null || text.isEmpty()) throw new Exception("empty message");
        SmsManager sm = null;
        if (Build.VERSION.SDK_INT >= 31) sm = c.getSystemService(SmsManager.class);
        if (sm == null) sm = SmsManager.getDefault();
        ArrayList<String> parts = sm.divideMessage(text);
        if (parts.size() > 1) sm.sendMultipartTextMessage(to.trim(), null, parts, null, null);
        else sm.sendTextMessage(to.trim(), null, text, null, null);
        return Boolean.TRUE; // Android records SMS sent by non-default apps in the Messages app automatically
    }

    // ---------------- calls ----------------

    static Object answer(Context c) throws Exception {
        need(c, Manifest.permission.ANSWER_PHONE_CALLS);
        TelecomManager tm = c.getSystemService(TelecomManager.class);
        if (tm == null) throw new Exception("no telephony on this device");
        tm.acceptRingingCall();
        return Boolean.TRUE;
    }

    static Object end(Context c) throws Exception {
        if (Build.VERSION.SDK_INT < 28) throw new Exception("ending calls remotely needs Android 9 or newer");
        need(c, Manifest.permission.ANSWER_PHONE_CALLS);
        TelecomManager tm = c.getSystemService(TelecomManager.class);
        if (tm == null) throw new Exception("no telephony on this device");
        return tm.endCall();
    }

    static Object dial(Context c, String number) throws Exception {
        need(c, Manifest.permission.CALL_PHONE);
        if (number == null || number.trim().isEmpty()) throw new Exception("no number");
        TelecomManager tm = c.getSystemService(TelecomManager.class);
        if (tm == null) throw new Exception("no telephony on this device");
        tm.placeCall(Uri.fromParts("tel", number.trim(), null), new Bundle());
        return Boolean.TRUE;
    }

    static JSONObject callLog(Context c, int limit) throws Exception {
        need(c, Manifest.permission.READ_CALL_LOG);
        if (limit <= 0) limit = 60;
        JSONArray arr = new JSONArray();
        String[] cols = {CallLog.Calls.NUMBER, CallLog.Calls.CACHED_NAME, CallLog.Calls.TYPE, CallLog.Calls.DATE, CallLog.Calls.DURATION};
        try (Cursor cur = c.getContentResolver().query(CallLog.Calls.CONTENT_URI, cols, null, null, CallLog.Calls.DATE + " DESC")) {
            while (cur != null && cur.moveToNext() && arr.length() < limit) {
                JSONObject o = new JSONObject();
                String num = cur.getString(0);
                o.put("number", num == null ? "" : num);
                String name = cur.getString(1);
                if (name == null || name.isEmpty()) name = contactName(c, num);
                o.put("name", name == null ? "" : name);
                int t = cur.getInt(2);
                String type;
                switch (t) {
                    case CallLog.Calls.INCOMING_TYPE: type = "incoming"; break;
                    case CallLog.Calls.OUTGOING_TYPE: type = "outgoing"; break;
                    case CallLog.Calls.MISSED_TYPE: type = "missed"; break;
                    case CallLog.Calls.VOICEMAIL_TYPE: type = "voicemail"; break;
                    case CallLog.Calls.REJECTED_TYPE: type = "rejected"; break;
                    case CallLog.Calls.BLOCKED_TYPE: type = "blocked"; break;
                    default: type = "other";
                }
                o.put("type", type);
                o.put("time", cur.getLong(3));
                o.put("duration", cur.getLong(4));
                arr.put(o);
            }
        }
        JSONObject o = new JSONObject();
        o.put("calls", arr);
        return o;
    }

    // ---------------- WhatsApp ----------------

    /** Opens the chat with the text filled in, then taps Send via the accessibility service. */
    static JSONObject whatsappSend(AgentService svc, String to, String text, boolean business) throws Exception {
        String digits = to == null ? "" : to.replaceAll("[^0-9]", "");
        if (digits.isEmpty()) throw new Exception("give a phone number with country code, e.g. +15145551234");
        if (text == null || text.isEmpty()) throw new Exception("empty message");
        String pkg = business ? "com.whatsapp.w4b" : "com.whatsapp";
        if (!installed(svc, pkg)) throw new Exception((business ? "WhatsApp Business" : "WhatsApp") + " is not installed on the phone");
        Intent i = new Intent(Intent.ACTION_VIEW,
                Uri.parse("https://api.whatsapp.com/send?phone=" + digits + "&text=" + Uri.encode(text)));
        i.setPackage(pkg);
        i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK | Intent.FLAG_ACTIVITY_CLEAR_TOP);
        ControlService cs = ControlService.instance;
        Context launcher = cs != null ? cs : svc;
        launcher.startActivity(i);
        JSONObject o = new JSONObject();
        if (cs == null) {
            o.put("sent", false);
            o.put("note", "WhatsApp opened with the message — tap Send on the phone (turn on remote touch in BridgeDesk to send automatically)");
            return o;
        }
        for (int k = 0; k < 48; k++) {
            Thread.sleep(250);
            if (cs.clickSend(pkg)) {
                o.put("sent", true);
                return o;
            }
        }
        o.put("sent", false);
        o.put("note", "WhatsApp opened with the message, but its Send button wasn't found — is the phone unlocked?");
        return o;
    }
}

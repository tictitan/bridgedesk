package com.bridgedesk.app;

import android.app.Activity;
import android.content.ClipData;
import android.content.ClipboardManager;
import android.content.Context;
import android.content.Intent;
import android.os.Bundle;
import android.widget.Toast;

import org.json.JSONObject;

/**
 * Sends the phone's clipboard (or shared text) to every PC in the fleet.
 * Android 10+ only lets the focused app read the clipboard, so this invisible activity flashes in
 * from the "Send clipboard" notification button, reads it once it has focus, sends it and closes.
 * It is also a Share target: Share → "Send to PC" from any app.
 */
public class ClipSendActivity extends Activity {
    private boolean done;

    @Override protected void onCreate(Bundle b) {
        super.onCreate(b);
        Intent i = getIntent();
        if (i != null && Intent.ACTION_SEND.equals(i.getAction())) {
            CharSequence t = i.getCharSequenceExtra(Intent.EXTRA_TEXT);
            if (t == null) t = i.getStringExtra(Intent.EXTRA_SUBJECT);
            send(t == null ? "" : t.toString());
        }
    }

    @Override public void onWindowFocusChanged(boolean hasFocus) {
        super.onWindowFocusChanged(hasFocus);
        if (!hasFocus || done) return;
        send(readClipboard(this));
    }

    static String readClipboard(Context c) {
        try {
            ClipboardManager cm = (ClipboardManager) c.getSystemService(Context.CLIPBOARD_SERVICE);
            if (cm == null || !cm.hasPrimaryClip()) return "";
            ClipData cd = cm.getPrimaryClip();
            if (cd == null || cd.getItemCount() == 0) return "";
            CharSequence t = cd.getItemAt(0).coerceToText(c);
            return t == null ? "" : t.toString();
        } catch (Exception e) {
            return "";
        }
    }

    private void send(String text) {
        done = true;
        boolean ok = sendToPc(text);
        String msg = text.isEmpty() ? "Clipboard is empty" : ok ? "Clipboard sent to your PCs" : "Not connected to a PC";
        Toast.makeText(this, msg, Toast.LENGTH_SHORT).show();
        finish();
    }

    /** Emit a clip.copied event; every hub in the fleet puts it on its clipboard (and Win+V history). */
    static boolean sendToPc(String text) {
        AgentService a = AgentService.instance;
        if (text == null || text.isEmpty() || a == null || !AgentService.connected) return false;
        if (!a.store().allowClipSync()) return false;
        try {
            JSONObject o = new JSONObject();
            o.put("sid", "clip");
            o.put("text", text);
            a.emit("clip.copied", o);
            Rpc.lastClip = text;
            return true;
        } catch (Exception e) {
            return false;
        }
    }
}

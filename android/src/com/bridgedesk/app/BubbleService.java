package com.bridgedesk.app;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.graphics.Color;
import android.graphics.PixelFormat;
import android.graphics.drawable.GradientDrawable;
import android.os.Build;
import android.os.Handler;
import android.os.IBinder;
import android.os.Looper;
import android.text.InputType;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.MotionEvent;
import android.view.View;
import android.view.ViewGroup;
import android.view.WindowManager;
import android.widget.Button;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;

import java.util.concurrent.ArrayBlockingQueue;

/**
 * The floating Aria bubble: a draggable chat-head drawn over every app (SYSTEM_ALERT_WINDOW).
 * Tap it to expand a mini chat that runs the on-device assistant ({@link Anthropic}) with the same
 * tools and Allow/Deny approvals as the main app — so Aria is reachable anywhere, even in a game.
 */
public class BubbleService extends Service {
    public static final String ACTION_STOP = "bubble_stop";
    private static final String CHAN = "aria_bubble";

    private WindowManager wm;
    private View bubble;
    private View panel;
    private LinearLayout log;
    private ScrollView logScroll;
    private EditText input;
    private Button send;
    private TextView current;      // assistant bubble being filled
    private boolean busy;
    private WindowManager.LayoutParams bubbleLp, panelLp;
    private final Handler ui = new Handler(Looper.getMainLooper());
    private Store store;
    private Anthropic aria;

    @Override public void onCreate() {
        super.onCreate();
        store = new Store(this);
        wm = (WindowManager) getSystemService(WINDOW_SERVICE);
        if (Build.VERSION.SDK_INT >= 29) startForeground(7, note(), ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE);
        else startForeground(7, note());
        addBubble();
    }

    @Override public int onStartCommand(Intent i, int flags, int startId) {
        if (i != null && ACTION_STOP.equals(i.getAction())) { stopSelf(); return START_NOT_STICKY; }
        return START_STICKY;
    }

    @Override public void onDestroy() {
        remove(panel); remove(bubble);
        super.onDestroy();
    }

    @Override public IBinder onBind(Intent i) { return null; }

    // ---- bubble ----
    private void addBubble() {
        TextView b = new TextView(this);
        b.setText("✦");
        b.setTextColor(Color.WHITE);
        b.setTextSize(TypedValue.COMPLEX_UNIT_SP, 22);
        b.setGravity(Gravity.CENTER);
        int sz = dp(52);
        GradientDrawable bg = new GradientDrawable();
        bg.setShape(GradientDrawable.OVAL);
        bg.setColor(0xFF2D6FD6);
        b.setBackground(bg);
        bubble = b;

        bubbleLp = new WindowManager.LayoutParams(sz, sz, overlayType(),
                WindowManager.LayoutParams.FLAG_NOT_FOCUSABLE
                        | WindowManager.LayoutParams.FLAG_NOT_TOUCH_MODAL
                        | WindowManager.LayoutParams.FLAG_LAYOUT_NO_LIMITS,
                PixelFormat.TRANSLUCENT);
        bubbleLp.gravity = Gravity.TOP | Gravity.START;
        bubbleLp.x = dp(12); bubbleLp.y = dp(140);

        b.setOnTouchListener(new View.OnTouchListener() {
            int ix, iy; float tx, ty; boolean moved;
            public boolean onTouch(View v, MotionEvent e) {
                switch (e.getAction()) {
                    case MotionEvent.ACTION_DOWN:
                        ix = bubbleLp.x; iy = bubbleLp.y; tx = e.getRawX(); ty = e.getRawY(); moved = false;
                        return true;
                    case MotionEvent.ACTION_MOVE:
                        int dx = (int) (e.getRawX() - tx), dy = (int) (e.getRawY() - ty);
                        if (Math.abs(dx) + Math.abs(dy) > dp(8)) moved = true;
                        bubbleLp.x = ix + dx; bubbleLp.y = iy + dy;
                        safeUpdate(bubble, bubbleLp);
                        return true;
                    case MotionEvent.ACTION_UP:
                        if (!moved) togglePanel();
                        return true;
                }
                return false;
            }
        });
        try { wm.addView(bubble, bubbleLp); } catch (Exception ignored) {}
    }

    // ---- panel ----
    private void togglePanel() {
        if (panel != null) { remove(panel); panel = null; return; }
        buildPanel();
    }

    private void buildPanel() {
        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(0xF20D1117);
        root.setPadding(dp(10), dp(10), dp(10), dp(10));

        LinearLayout bar = new LinearLayout(this);
        bar.setOrientation(LinearLayout.HORIZONTAL);
        bar.setGravity(Gravity.CENTER_VERTICAL);
        TextView title = new TextView(this);
        title.setText("Aria");
        title.setTextColor(0xFFE6EDF3); title.setTextSize(TypedValue.COMPLEX_UNIT_SP, 16);
        title.setLayoutParams(new LinearLayout.LayoutParams(0, -2, 1f));
        bar.addView(title);
        bar.addView(mini("Brief my day", v -> ask(
                "Organize my day: check my calendar (read_calendar) and current notifications, then give me a short prioritized plan and send it to me with notify_me.")));
        bar.addView(space());
        bar.addView(mini("✕", v -> { remove(panel); panel = null; }));
        root.addView(bar);

        logScroll = new ScrollView(this);
        logScroll.setLayoutParams(new LinearLayout.LayoutParams(-1, dp(260)));
        log = new LinearLayout(this); log.setOrientation(LinearLayout.VERTICAL);
        logScroll.addView(log);
        root.addView(logScroll);
        if (!store.hasApiKey())
            note("Set your Anthropic API key in the Aria app (⚙ → Assistant) to use the bubble on-device.");

        LinearLayout row = new LinearLayout(this);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setGravity(Gravity.CENTER_VERTICAL);
        input = new EditText(this);
        input.setHint("Ask Aria…");
        input.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_FLAG_CAP_SENTENCES);
        input.setTextColor(0xFFE6EDF3); input.setHintTextColor(0xFF6B7684);
        input.setBackgroundColor(0xFF161B22); input.setPadding(dp(10), dp(8), dp(10), dp(8));
        input.setLayoutParams(new LinearLayout.LayoutParams(0, -2, 1f));
        send = mini("Send", v -> onSend());
        row.addView(input); row.addView(send);
        root.addView(row);

        panel = root;
        panelLp = new WindowManager.LayoutParams(
                Math.min(dp(340), getResources().getDisplayMetrics().widthPixels - dp(24)),
                WindowManager.LayoutParams.WRAP_CONTENT, overlayType(),
                WindowManager.LayoutParams.FLAG_NOT_TOUCH_MODAL, // focusable so the keyboard works
                PixelFormat.TRANSLUCENT);
        panelLp.gravity = Gravity.TOP | Gravity.START;
        panelLp.x = dp(12); panelLp.y = dp(140) + dp(60);
        panelLp.softInputMode = WindowManager.LayoutParams.SOFT_INPUT_ADJUST_RESIZE;
        try { wm.addView(panel, panelLp); } catch (Exception ignored) {}
    }

    private void onSend() {
        if (busy || input == null) return;
        String t = input.getText().toString().trim();
        if (t.isEmpty()) return;
        input.setText("");
        ask(t);
    }

    private void ask(String prompt) {
        if (!store.hasApiKey()) { note("No API key set — open the Aria app → ⚙ → Assistant."); return; }
        if (busy) return;
        busy = true; send.setEnabled(false);
        bubble(true, prompt);
        current = bubble(false, "…");
        if (aria == null) aria = new Anthropic();
        Anthropic.Gate gate = (tool, inputJson) -> {
            final ArrayBlockingQueue<Boolean> q = new ArrayBlockingQueue<>(1);
            ui.post(() -> approvalRow(tool, inputJson, allow -> q.offer(allow)));
            try { return q.take(); } catch (Exception e) { return false; }
        };
        aria.ask(this, store, prompt, new Anthropic.Listener() {
            public void onDelta(String s) { ui.post(() -> { if (current == null) current = bubble(false, ""); String c = current.getText().toString(); if (c.equals("…")) c = ""; current.setText(c.isEmpty() ? s : c + "\n" + s); down(); }); }
            public void onTool(String name) { ui.post(() -> { toolLine(name); current = null; }); }
            public void onToolResult(String s) { ui.post(() -> toolLine("→ " + s)); }
            public void onDone(String s) { ui.post(() -> { if (current != null && "…".equals(current.getText().toString())) current.setText(s == null || s.isEmpty() ? "(no reply)" : s); current = null; busy = false; if (send != null) send.setEnabled(true); down(); }); }
            public void onError(String m) { ui.post(() -> { note("⚠ " + m); current = null; busy = false; if (send != null) send.setEnabled(true); }); }
        }, gate);
    }

    // ---- panel views ----
    private TextView bubble(boolean user, String text) {
        TextView t = new TextView(this);
        t.setText(text);
        t.setTextColor(user ? 0xFFFFFFFF : 0xFFE6EDF3);
        t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 14);
        t.setBackgroundColor(user ? 0xFF2D6FD6 : 0xFF161B22);
        t.setPadding(dp(10), dp(7), dp(10), dp(7));
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(-2, -2);
        lp.gravity = user ? Gravity.END : Gravity.START; lp.topMargin = dp(5);
        lp.leftMargin = user ? dp(30) : 0; lp.rightMargin = user ? 0 : dp(30);
        t.setLayoutParams(lp);
        if (log != null) log.addView(t);
        down();
        return t;
    }
    private void toolLine(String s) {
        TextView t = new TextView(this);
        t.setText("⚙ " + s); t.setTextColor(0xFF8B98A9); t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 11);
        if (log != null) log.addView(t);
        down();
    }
    private void note(String s) {
        TextView t = new TextView(this);
        t.setText(s); t.setTextColor(0xFF6B7684); t.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12);
        t.setPadding(0, dp(4), 0, dp(4));
        if (log != null) log.addView(t);
        down();
    }
    private void approvalRow(String tool, String inputJson, final java.util.function.Consumer<Boolean> decide) {
        LinearLayout card = new LinearLayout(this);
        card.setOrientation(LinearLayout.VERTICAL);
        card.setBackgroundColor(0xFF2A2113); card.setPadding(dp(10), dp(8), dp(10), dp(8));
        LinearLayout.LayoutParams clp = new LinearLayout.LayoutParams(-1, -2); clp.topMargin = dp(6); card.setLayoutParams(clp);
        TextView h = new TextView(this);
        h.setText("⚠ Allow " + tool + "?"); h.setTextColor(0xFFF0883E); h.setTextSize(TypedValue.COMPLEX_UNIT_SP, 13);
        card.addView(h);
        if (inputJson != null && !inputJson.equals("{}")) { TextView d = new TextView(this); d.setText(inputJson.length() > 200 ? inputJson.substring(0, 200) + "…" : inputJson); d.setTextColor(0xFFE6EDF3); d.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12); card.addView(d); }
        LinearLayout btns = new LinearLayout(this);
        Button allow = mini("Allow", null), deny = mini("Deny", null);
        View.OnClickListener cl = v -> { boolean ok = v == allow; decide.accept(ok); allow.setEnabled(false); deny.setEnabled(false); h.setText((ok ? "✓ Allowed " : "✕ Denied ") + tool); };
        allow.setOnClickListener(cl); deny.setOnClickListener(cl);
        btns.addView(allow); btns.addView(space()); btns.addView(deny);
        card.addView(btns);
        if (log != null) log.addView(card);
        down();
    }

    private Button mini(String t, View.OnClickListener l) {
        Button b = new Button(this); b.setText(t); b.setAllCaps(false);
        b.setTextSize(TypedValue.COMPLEX_UNIT_SP, 12); b.setTextColor(0xFFE6EDF3);
        b.setBackgroundColor(0xFF1C2230); b.setPadding(dp(10), dp(4), dp(10), dp(4));
        if (l != null) b.setOnClickListener(l);
        return b;
    }
    private View space() { View v = new View(this); v.setLayoutParams(new LinearLayout.LayoutParams(dp(6), 1)); return v; }
    private void down() { if (logScroll != null) logScroll.post(() -> logScroll.fullScroll(View.FOCUS_DOWN)); }

    // ---- infra ----
    private int overlayType() {
        return Build.VERSION.SDK_INT >= Build.VERSION_CODES.O
                ? WindowManager.LayoutParams.TYPE_APPLICATION_OVERLAY
                : WindowManager.LayoutParams.TYPE_PHONE;
    }
    private void safeUpdate(View v, WindowManager.LayoutParams lp) { try { if (v != null) wm.updateViewLayout(v, lp); } catch (Exception ignored) {} }
    private void remove(View v) { try { if (v != null) wm.removeView(v); } catch (Exception ignored) {} }
    private int dp(int d) { return (int) (d * getResources().getDisplayMetrics().density); }

    private Notification note() {
        NotificationManager nm = getSystemService(NotificationManager.class);
        if (Build.VERSION.SDK_INT >= 26) nm.createNotificationChannel(new NotificationChannel(CHAN, "Aria bubble", NotificationManager.IMPORTANCE_MIN));
        Intent stop = new Intent(this, BubbleService.class).setAction(ACTION_STOP);
        return new Notification.Builder(this, CHAN)
                .setContentTitle("Aria is floating")
                .setContentText("Tap the bubble anywhere to ask Aria.")
                .setSmallIcon(android.R.drawable.ic_menu_compass)
                .addAction(new Notification.Action.Builder(null, "Stop", android.app.PendingIntent.getService(this, 0, stop, android.app.PendingIntent.FLAG_IMMUTABLE)).build())
                .setOngoing(true)
                .build();
    }

    public static void start(Context c) {
        Intent i = new Intent(c, BubbleService.class);
        if (Build.VERSION.SDK_INT >= 26) c.startForegroundService(i); else c.startService(i);
    }
    public static void stop(Context c) { c.startService(new Intent(c, BubbleService.class).setAction(ACTION_STOP)); }
}

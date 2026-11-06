package com.bridgedesk.app;

import android.accessibilityservice.AccessibilityService;
import android.accessibilityservice.GestureDescription;
import android.graphics.Path;
import android.os.Bundle;
import android.view.accessibility.AccessibilityEvent;
import android.view.accessibility.AccessibilityNodeInfo;

/**
 * Injects taps, swipes and text so the PC can drive this phone remotely.
 * Android routes all synthetic input through an accessibility service; the OS blocks it on a
 * secure lock screen, so remote touch works once the phone is unlocked (screen viewing works either way).
 */
public class ControlService extends AccessibilityService {
    static ControlService instance;

    @Override public void onServiceConnected() { instance = this; }
    @Override public void onAccessibilityEvent(AccessibilityEvent e) {}
    @Override public void onInterrupt() {}
    @Override public boolean onUnbind(android.content.Intent i) { instance = null; return super.onUnbind(i); }

    public static boolean available() { return instance != null; }

    /** Tap at absolute pixel coordinates. */
    public void tap(float x, float y) {
        Path p = new Path(); p.moveTo(x, y);
        dispatch(new GestureDescription.Builder()
                .addStroke(new GestureDescription.StrokeDescription(p, 0, 60)).build());
    }

    public void longPress(float x, float y) {
        Path p = new Path(); p.moveTo(x, y);
        dispatch(new GestureDescription.Builder()
                .addStroke(new GestureDescription.StrokeDescription(p, 0, 600)).build());
    }

    public void swipe(float x1, float y1, float x2, float y2, int ms) {
        Path p = new Path(); p.moveTo(x1, y1); p.lineTo(x2, y2);
        dispatch(new GestureDescription.Builder()
                .addStroke(new GestureDescription.StrokeDescription(p, 0, Math.max(60, ms))).build());
    }

    private void dispatch(GestureDescription g) {
        try { dispatchGesture(g, null, null); } catch (Exception ignored) {}
    }

    /** Type text into the currently focused editable field. */
    public boolean typeText(String text) {
        AccessibilityNodeInfo node = findFocusedEditable();
        if (node == null) return false;
        Bundle args = new Bundle();
        CharSequence existing = node.getText();
        String combined = (existing == null ? "" : existing.toString()) + text;
        args.putCharSequence(AccessibilityNodeInfo.ACTION_ARGUMENT_SET_TEXT_CHARSEQUENCE, combined);
        boolean ok = node.performAction(AccessibilityNodeInfo.ACTION_SET_TEXT, args);
        node.recycle();
        return ok;
    }

    /** Delete the last character of the focused editable field. */
    public boolean deleteChar() {
        AccessibilityNodeInfo node = findFocusedEditable();
        if (node == null) return false;
        CharSequence existing = node.getText();
        String s = existing == null ? "" : existing.toString();
        if (s.isEmpty()) { node.recycle(); return true; }
        Bundle args = new Bundle();
        args.putCharSequence(AccessibilityNodeInfo.ACTION_ARGUMENT_SET_TEXT_CHARSEQUENCE, s.substring(0, s.length() - 1));
        boolean ok = node.performAction(AccessibilityNodeInfo.ACTION_SET_TEXT, args);
        node.recycle();
        return ok;
    }

    /** Click WhatsApp's Send button (by view id, falling back to its "Send" label). */
    public boolean clickSend(String pkg) {
        AccessibilityNodeInfo root = getRootInActiveWindow();
        if (root == null) return false;
        java.util.List<AccessibilityNodeInfo> nodes = root.findAccessibilityNodeInfosByViewId(pkg + ":id/send");
        if (nodes == null || nodes.isEmpty()) nodes = root.findAccessibilityNodeInfosByText("Send");
        if (nodes == null) return false;
        for (AccessibilityNodeInfo n : nodes) {
            AccessibilityNodeInfo t = n;
            while (t != null && !t.isClickable()) t = t.getParent();
            if (t != null && t.isEnabled() && t.performAction(AccessibilityNodeInfo.ACTION_CLICK)) return true;
        }
        return false;
    }

    private AccessibilityNodeInfo findFocusedEditable() {
        AccessibilityNodeInfo root = getRootInActiveWindow();
        if (root == null) return null;
        AccessibilityNodeInfo f = root.findFocus(AccessibilityNodeInfo.FOCUS_INPUT);
        if (f != null && f.isEditable()) return f;
        return null;
    }

    /** Map named keys to global navigation actions available to an accessibility service. */
    public boolean globalKey(String key) {
        int action;
        switch (key) {
            case "home": action = GLOBAL_ACTION_HOME; break;
            case "back": action = GLOBAL_ACTION_BACK; break;
            case "recents": action = GLOBAL_ACTION_RECENTS; break;
            case "notifications": action = GLOBAL_ACTION_NOTIFICATIONS; break;
            case "quicksettings": action = GLOBAL_ACTION_QUICK_SETTINGS; break;
            case "power": action = GLOBAL_ACTION_POWER_DIALOG; break;
            case "lock":
                if (android.os.Build.VERSION.SDK_INT >= 28) action = GLOBAL_ACTION_LOCK_SCREEN; else return false;
                break;
            default: return false;
        }
        return performGlobalAction(action);
    }
}

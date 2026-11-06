package com.bridgedesk.app;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

/** Re-links the phone after reboot if the agent was enabled. */
public class BootReceiver extends BroadcastReceiver {
    @Override public void onReceive(Context c, Intent i) {
        if (new Store(c).agentEnabled()) AgentService.startAgent(c);
    }
}

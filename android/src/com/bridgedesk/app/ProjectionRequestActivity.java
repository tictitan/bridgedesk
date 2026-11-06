package com.bridgedesk.app;

import android.app.Activity;
import android.content.Intent;
import android.media.projection.MediaProjectionManager;
import android.os.Build;
import android.os.Bundle;
import android.widget.Toast;

/** Transparent activity that asks the user to approve screen capture, then hands the grant to AgentService. */
public class ProjectionRequestActivity extends Activity {
    private static final int REQ = 9001;

    @Override protected void onCreate(Bundle b) {
        super.onCreate(b);
        MediaProjectionManager mpm = (MediaProjectionManager) getSystemService(MEDIA_PROJECTION_SERVICE);
        try {
            startActivityForResult(mpm.createScreenCaptureIntent(), REQ);
        } catch (Exception e) {
            Toast.makeText(this, "Screen capture unavailable", Toast.LENGTH_SHORT).show();
            finish();
        }
    }

    @Override protected void onActivityResult(int req, int res, Intent data) {
        super.onActivityResult(req, res, data);
        if (req == REQ && res == RESULT_OK && data != null) {
            Intent i = new Intent(this, AgentService.class).setAction(AgentService.ACTION_PROJECTION);
            i.putExtra("code", res);
            i.putExtra("data", data);
            if (Build.VERSION.SDK_INT >= 26) startForegroundService(i); else startService(i);
        } else {
            Toast.makeText(this, "Screen capture not granted", Toast.LENGTH_SHORT).show();
        }
        finish();
    }
}

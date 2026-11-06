package com.bridgedesk.app;

import android.content.Context;
import android.graphics.Bitmap;
import android.graphics.PixelFormat;
import android.hardware.display.DisplayManager;
import android.hardware.display.VirtualDisplay;
import android.media.Image;
import android.media.ImageReader;
import android.media.projection.MediaProjection;
import android.os.Handler;
import android.os.HandlerThread;
import android.util.DisplayMetrics;
import android.view.WindowManager;

import java.io.ByteArrayOutputStream;
import java.nio.ByteBuffer;

/**
 * Captures the device screen through MediaProjection into JPEG frames.
 * Keeps producing frames while the screen is locked, so the PC can watch a locked phone
 * (synthetic input past a secure keyguard is still blocked by Android — see ControlService).
 */
public class ScreenCapture {
    private final Context ctx;
    private MediaProjection projection;
    private ImageReader reader;
    private VirtualDisplay vdisplay;
    private HandlerThread thread;
    private Handler handler;
    private int width, height, density;
    private volatile byte[] lastJpeg;
    private volatile int lastW, lastH;
    private int targetMaxW = 1280;
    private int quality = 60;

    public ScreenCapture(Context c) { this.ctx = c; }

    public synchronized void start(MediaProjection mp) {
        this.projection = mp;
        WindowManager wm = (WindowManager) ctx.getSystemService(Context.WINDOW_SERVICE);
        DisplayMetrics dm = new DisplayMetrics();
        wm.getDefaultDisplay().getRealMetrics(dm);
        width = dm.widthPixels;
        height = dm.heightPixels;
        density = dm.densityDpi;

        // Downscale the virtual display to keep frames small/fast.
        int vw = width, vh = height;
        if (width > targetMaxW) {
            vw = targetMaxW;
            vh = (int) ((long) height * targetMaxW / width);
        }
        vw &= ~1; vh &= ~1;

        thread = new HandlerThread("bd-capture");
        thread.start();
        handler = new Handler(thread.getLooper());

        reader = ImageReader.newInstance(vw, vh, PixelFormat.RGBA_8888, 2);
        final int fvw = vw, fvh = vh;
        reader.setOnImageAvailableListener(r -> {
            Image img = null;
            try {
                img = r.acquireLatestImage();
                if (img == null) return;
                Image.Plane[] planes = img.getPlanes();
                ByteBuffer buf = planes[0].getBuffer();
                int pixelStride = planes[0].getPixelStride();
                int rowStride = planes[0].getRowStride();
                int rowPadding = rowStride - pixelStride * fvw;
                Bitmap bmp = Bitmap.createBitmap(fvw + rowPadding / pixelStride, fvh, Bitmap.Config.ARGB_8888);
                bmp.copyPixelsFromBuffer(buf);
                Bitmap cropped = (rowPadding == 0) ? bmp : Bitmap.createBitmap(bmp, 0, 0, fvw, fvh);
                ByteArrayOutputStream bos = new ByteArrayOutputStream();
                cropped.compress(Bitmap.CompressFormat.JPEG, quality, bos);
                lastJpeg = bos.toByteArray();
                lastW = fvw; lastH = fvh;
                if (cropped != bmp) cropped.recycle();
                bmp.recycle();
            } catch (Exception ignored) {
            } finally {
                if (img != null) img.close();
            }
        }, handler);

        vdisplay = projection.createVirtualDisplay("bd-screen", vw, vh, density,
                DisplayManager.VIRTUAL_DISPLAY_FLAG_AUTO_MIRROR,
                reader.getSurface(), null, handler);
    }

    public void configure(int maxW, int q) {
        if (maxW > 0) this.targetMaxW = maxW;
        if (q > 0) this.quality = q;
    }

    public byte[] latestJpeg() { return lastJpeg; }
    public int realWidth() { return width; }
    public int realHeight() { return height; }
    public int frameWidth() { return lastW; }
    public int frameHeight() { return lastH; }

    public synchronized void stop() {
        try { if (vdisplay != null) vdisplay.release(); } catch (Exception ignored) {}
        try { if (reader != null) reader.close(); } catch (Exception ignored) {}
        try { if (projection != null) projection.stop(); } catch (Exception ignored) {}
        try { if (thread != null) thread.quitSafely(); } catch (Exception ignored) {}
        vdisplay = null; reader = null; projection = null;
        lastJpeg = null;
    }

    public boolean running() { return projection != null; }
}

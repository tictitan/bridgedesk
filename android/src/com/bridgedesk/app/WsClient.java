package com.bridgedesk.app;

import java.io.*;
import java.net.*;
import java.security.MessageDigest;
import java.security.SecureRandom;
import java.security.cert.X509Certificate;
import java.util.*;
import javax.net.ssl.*;

/**
 * Minimal RFC 6455 WebSocket client over a TLS socket, with the server certificate
 * verified by a pinned SHA-256 fingerprint instead of a CA chain (the hub is self-signed).
 * Framework-only; no third-party dependencies so the APK builds without Gradle.
 */
public class WsClient {

    public interface Listener {
        void onOpen();
        void onText(String msg);
        void onClose(String reason);
    }

    private Socket socket;
    private InputStream in;
    private OutputStream out;
    private final SecureRandom rnd = new SecureRandom();
    private volatile boolean open = false;
    private final Object writeLock = new Object();

    public boolean isOpen() { return open; }

    /** Connect to wss://host:port/path with ?key, pinning the given fingerprint (hex, spaces ignored). */
    public void connect(String host, int port, String path, String pinFp, Listener l) throws Exception {
        SSLContext ctx = SSLContext.getInstance("TLS");
        ctx.init(null, new TrustManager[]{ new PinnedTrust(pinFp) }, new SecureRandom());
        SSLSocket s = (SSLSocket) ctx.getSocketFactory().createSocket();
        s.connect(new InetSocketAddress(host, port), 15000);
        s.setSoTimeout(0);
        s.startHandshake();
        this.socket = s;
        this.in = new BufferedInputStream(s.getInputStream());
        this.out = new BufferedOutputStream(s.getOutputStream());

        byte[] nonce = new byte[16];
        rnd.nextBytes(nonce);
        String keyB64 = Base64.getEncoder().encodeToString(nonce);
        String req = "GET " + path + " HTTP/1.1\r\n" +
                "Host: " + host + ":" + port + "\r\n" +
                "Upgrade: websocket\r\n" +
                "Connection: Upgrade\r\n" +
                "Sec-WebSocket-Key: " + keyB64 + "\r\n" +
                "Sec-WebSocket-Version: 13\r\n\r\n";
        out.write(req.getBytes("UTF-8"));
        out.flush();

        String statusLine = readLine(in);
        if (statusLine == null || !statusLine.contains(" 101")) {
            // drain and fail
            throw new IOException("handshake failed: " + statusLine);
        }
        while (true) {
            String line = readLine(in);
            if (line == null || line.isEmpty()) break;
        }
        open = true;
        l.onOpen();
        readLoop(l);
    }

    private void readLoop(Listener l) {
        try {
            ByteArrayOutputStream frag = new ByteArrayOutputStream();
            int fragOp = 0;
            while (open) {
                int b0 = in.read();
                if (b0 < 0) break;
                int b1 = in.read();
                if (b1 < 0) break;
                boolean fin = (b0 & 0x80) != 0;
                int opcode = b0 & 0x0F;
                boolean masked = (b1 & 0x80) != 0;
                long len = b1 & 0x7F;
                if (len == 126) {
                    len = ((long) in.read() << 8) | in.read();
                } else if (len == 127) {
                    len = 0;
                    for (int i = 0; i < 8; i++) len = (len << 8) | in.read();
                }
                byte[] mask = new byte[4];
                if (masked) readFully(in, mask, 4);
                byte[] payload = new byte[(int) len];
                readFully(in, payload, (int) len);
                if (masked) for (int i = 0; i < payload.length; i++) payload[i] ^= mask[i & 3];

                switch (opcode) {
                    case 0x1: // text
                    case 0x2: // binary
                    case 0x0: // continuation
                        if (opcode != 0x0) fragOp = opcode;
                        frag.write(payload);
                        if (fin) {
                            String msg = new String(frag.toByteArray(), "UTF-8");
                            frag.reset();
                            l.onText(msg);
                        }
                        break;
                    case 0x8: // close
                        sendFrame(0x8, new byte[0]);
                        open = false;
                        break;
                    case 0x9: // ping
                        sendFrame(0xA, payload);
                        break;
                    case 0xA: // pong
                        break;
                }
            }
        } catch (Exception e) {
            // fall through to close
        } finally {
            open = false;
            l.onClose("closed");
            try { socket.close(); } catch (Exception ignored) {}
        }
    }

    public void sendText(String msg) throws IOException {
        byte[] data = msg.getBytes("UTF-8");
        sendFrame(0x1, data);
    }

    private void sendFrame(int opcode, byte[] payload) throws IOException {
        if (!open && opcode != 0x8) throw new IOException("closed");
        synchronized (writeLock) {
            ByteArrayOutputStream h = new ByteArrayOutputStream();
            h.write(0x80 | opcode);
            int len = payload.length;
            byte[] mask = new byte[4];
            rnd.nextBytes(mask);
            if (len < 126) {
                h.write(0x80 | len);
            } else if (len < 65536) {
                h.write(0x80 | 126);
                h.write((len >> 8) & 0xFF);
                h.write(len & 0xFF);
            } else {
                h.write(0x80 | 127);
                for (int i = 7; i >= 0; i--) h.write((int) ((((long) len) >> (8 * i)) & 0xFF));
            }
            h.write(mask);
            byte[] masked = new byte[len];
            for (int i = 0; i < len; i++) masked[i] = (byte) (payload[i] ^ mask[i & 3]);
            out.write(h.toByteArray());
            out.write(masked);
            out.flush();
        }
    }

    public void close() {
        try { sendFrame(0x8, new byte[0]); } catch (Exception ignored) {}
        open = false;
        try { socket.close(); } catch (Exception ignored) {}
    }

    private static String readLine(InputStream in) throws IOException {
        ByteArrayOutputStream bos = new ByteArrayOutputStream();
        int prev = -1, c;
        while ((c = in.read()) >= 0) {
            if (prev == '\r' && c == '\n') {
                byte[] b = bos.toByteArray();
                return new String(b, 0, b.length - 1, "UTF-8");
            }
            bos.write(c);
            prev = c;
        }
        return bos.size() == 0 ? null : bos.toString("UTF-8");
    }

    private static void readFully(InputStream in, byte[] buf, int len) throws IOException {
        int off = 0;
        while (off < len) {
            int n = in.read(buf, off, len - off);
            if (n < 0) throw new EOFException();
            off += n;
        }
    }

    /** Trust manager that accepts exactly the certificate whose SHA-256 matches the pin. */
    static class PinnedTrust implements X509TrustManager {
        final String want;
        PinnedTrust(String fp) { this.want = fp == null ? "" : fp.replace(" ", "").toUpperCase(Locale.US); }
        public void checkClientTrusted(X509Certificate[] c, String t) {}
        public void checkServerTrusted(X509Certificate[] chain, String t) throws java.security.cert.CertificateException {
            if (want.isEmpty()) return; // trust-on-first-use handled by caller
            try {
                MessageDigest md = MessageDigest.getInstance("SHA-256");
                for (X509Certificate cert : chain) {
                    byte[] d = md.digest(cert.getEncoded());
                    StringBuilder sb = new StringBuilder();
                    for (byte x : d) sb.append(String.format("%02X", x));
                    if (sb.toString().equals(want)) return;
                }
            } catch (Exception e) {
                throw new java.security.cert.CertificateException(e);
            }
            throw new java.security.cert.CertificateException("fingerprint mismatch");
        }
        public X509Certificate[] getAcceptedIssuers() { return new X509Certificate[0]; }
    }
}

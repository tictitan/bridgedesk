package com.bridgedesk.app;

import android.content.Context;
import android.content.Intent;
import android.net.Uri;
import android.util.Base64;

import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.net.ServerSocket;
import java.net.Socket;
import java.net.URL;
import java.net.URLEncoder;
import java.security.MessageDigest;
import java.security.SecureRandom;
import javax.net.ssl.HttpsURLConnection;

/**
 * Sign in to Google without Google Play Services, using the standard "native app" flow:
 * a loopback redirect on 127.0.0.1 + PKCE — exactly what the Aria desktop app does. The phone
 * opens Google's own sign-in page in the browser (company SSO / 2-step included), catches the
 * redirect on a short-lived local socket, and exchanges the code for tokens. We keep the
 * refresh token and mint a fresh ID token (a signed JWT with your verified email) whenever the
 * hub needs one. The hub verifies that JWT, so only your Google account(s) get in.
 */
public class GoogleAuth {

    public interface SignInCb { void onResult(String email); void onError(String msg); }

    private static final String AUTH = "https://accounts.google.com/o/oauth2/v2/auth";
    private static final String TOKEN = "https://oauth2.googleapis.com/token";
    private static final String SCOPE = "openid email";

    /** Interactive sign-in. Opens the browser; completes on a background thread. */
    public static void signIn(final Context ctx, final Store st, final SignInCb cb) {
        final String clientId = st.gClientId();
        if (clientId.isEmpty()) { cb.onError("Enter your Google OAuth client ID first"); return; }
        new Thread(() -> {
            ServerSocket server = null;
            try {
                server = new ServerSocket(0, 1, java.net.InetAddress.getByName("127.0.0.1"));
                server.setSoTimeout(5 * 60 * 1000); // 5 min to finish in the browser
                int port = server.getLocalPort();
                String redirect = "http://127.0.0.1:" + port;

                byte[] vb = new byte[48]; new SecureRandom().nextBytes(vb);
                String verifier = b64url(vb);
                String challenge = b64url(MessageDigest.getInstance("SHA-256").digest(verifier.getBytes("UTF-8")));

                String url = AUTH + "?client_id=" + enc(clientId)
                        + "&redirect_uri=" + enc(redirect)
                        + "&response_type=code&scope=" + enc(SCOPE)
                        + "&code_challenge=" + enc(challenge) + "&code_challenge_method=S256"
                        + "&access_type=offline&prompt=consent";
                Intent i = new Intent(Intent.ACTION_VIEW, Uri.parse(url)).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
                ctx.startActivity(i);

                String code = awaitCode(server);
                if (code == null) { cb.onError("Sign-in was not completed"); return; }

                JSONObject tok = postForm(TOKEN,
                        "code=" + enc(code)
                        + "&client_id=" + enc(clientId)
                        + "&client_secret=" + enc(st.gClientSecret())
                        + "&code_verifier=" + enc(verifier)
                        + "&redirect_uri=" + enc(redirect)
                        + "&grant_type=authorization_code");
                String idTok = tok.optString("id_token", "");
                String refresh = tok.optString("refresh_token", "");
                long exp = expOf(idTok);
                String email = emailOf(idTok);
                if (idTok.isEmpty() || refresh.isEmpty()) {
                    cb.onError(tok.optString("error_description", tok.optString("error", "no tokens returned")));
                    return;
                }
                st.setGoogleTokens(refresh, idTok, exp, email);
                cb.onResult(email);
            } catch (Exception e) {
                cb.onError(e.getMessage() == null ? e.toString() : e.getMessage());
            } finally {
                if (server != null) try { server.close(); } catch (Exception ignored) {}
            }
        }, "google-signin").start();
    }

    /**
     * Return a currently-valid ID token for the hub, refreshing via the refresh token when the
     * cached one is missing or within 5 minutes of expiry. Returns "" if Google isn't set up.
     * Call this off the main thread (it may hit the network).
     */
    public static synchronized String freshIdToken(Store st) {
        if (!st.googleConfigured()) return "";
        long now = System.currentTimeMillis();
        String cached = st.gIdToken();
        if (!cached.isEmpty() && st.gIdExpMs() - now > 5 * 60 * 1000) return cached;
        try {
            JSONObject tok = postForm(TOKEN,
                    "client_id=" + enc(st.gClientId())
                    + "&client_secret=" + enc(st.gClientSecret())
                    + "&refresh_token=" + enc(st.gRefreshToken())
                    + "&grant_type=refresh_token");
            String idTok = tok.optString("id_token", "");
            if (idTok.isEmpty()) return cached; // best effort; keep the old one
            st.setGoogleTokens(null, idTok, expOf(idTok), emailOf(idTok));
            return idTok;
        } catch (Exception e) {
            return cached;
        }
    }

    // ---- helpers ----

    private static String awaitCode(ServerSocket server) throws Exception {
        try (Socket s = server.accept()) {
            BufferedReader r = new BufferedReader(new InputStreamReader(s.getInputStream(), "UTF-8"));
            String line = r.readLine(); // e.g. "GET /?code=...&scope=... HTTP/1.1"
            String code = null, err = null;
            if (line != null && line.startsWith("GET ")) {
                String path = line.split(" ")[1];
                Uri u = Uri.parse("http://x" + path);
                code = u.getQueryParameter("code");
                err = u.getQueryParameter("error");
            }
            String body = "<html><body style='font-family:sans-serif;background:#0D1117;color:#E6EDF3;padding:40px'>"
                    + (code != null ? "✓ Signed in to Aria. You can close this tab and return to the app."
                                    : "Sign-in failed" + (err != null ? ": " + err : "") + ".")
                    + "</body></html>";
            OutputStream out = s.getOutputStream();
            out.write(("HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nConnection: close\r\nContent-Length: "
                    + body.getBytes("UTF-8").length + "\r\n\r\n" + body).getBytes("UTF-8"));
            out.flush();
            return code;
        }
    }

    private static JSONObject postForm(String url, String form) throws Exception {
        HttpsURLConnection c = (HttpsURLConnection) new URL(url).openConnection();
        c.setRequestMethod("POST");
        c.setDoOutput(true);
        c.setConnectTimeout(15000);
        c.setReadTimeout(20000);
        c.setRequestProperty("Content-Type", "application/x-www-form-urlencoded");
        try (OutputStream os = c.getOutputStream()) { os.write(form.getBytes("UTF-8")); }
        int code = c.getResponseCode();
        java.io.InputStream in = code < 400 ? c.getInputStream() : c.getErrorStream();
        StringBuilder sb = new StringBuilder();
        BufferedReader br = new BufferedReader(new InputStreamReader(in, "UTF-8"));
        for (int ch; (ch = br.read()) >= 0; ) sb.append((char) ch);
        c.disconnect();
        return new JSONObject(sb.toString());
    }

    private static long expOf(String jwt) {
        try {
            JSONObject p = payload(jwt);
            return p.optLong("exp", 0) * 1000L;
        } catch (Exception e) { return 0; }
    }
    private static String emailOf(String jwt) {
        try { return payload(jwt).optString("email", ""); } catch (Exception e) { return ""; }
    }
    private static JSONObject payload(String jwt) throws Exception {
        String[] parts = jwt.split("\\.");
        byte[] b = Base64.decode(parts[1], Base64.URL_SAFE | Base64.NO_PADDING | Base64.NO_WRAP);
        return new JSONObject(new String(b, "UTF-8"));
    }
    private static String b64url(byte[] b) { return Base64.encodeToString(b, Base64.URL_SAFE | Base64.NO_PADDING | Base64.NO_WRAP); }
    private static String enc(String s) { try { return URLEncoder.encode(s == null ? "" : s, "UTF-8"); } catch (Exception e) { return s; } }
}

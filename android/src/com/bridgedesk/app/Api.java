package com.bridgedesk.app;

import java.io.InputStream;
import java.io.OutputStream;
import java.net.URL;
import java.net.URLEncoder;
import java.security.SecureRandom;
import javax.net.ssl.HttpsURLConnection;
import javax.net.ssl.SSLContext;
import javax.net.ssl.TrustManager;

/**
 * Tiny pinned-TLS REST client for the hub's /api/* endpoints, reusing the same SHA-256
 * certificate pin as {@link WsClient} and attaching the access key (and Google ID token, if the
 * hub requires sign-in). Used by the dashboard to read /api/devices, /api/notifications, /api/models.
 */
public class Api {

    /** GET <path> and return the response body, or throw. Call off the main thread. */
    public static String get(Store st, String path) throws Exception {
        return send(st, "GET", path, null);
    }

    /** POST JSON to <path>. */
    public static String post(Store st, String path, String json) throws Exception {
        return send(st, "POST", path, json);
    }

    private static String send(Store st, String method, String path, String json) throws Exception {
        String sep = path.contains("?") ? "&" : "?";
        String url = "https://" + st.host() + ":" + st.port() + path + sep + "key=" + enc(st.key());
        String gid = GoogleAuth.freshIdToken(st);
        if (!gid.isEmpty()) url += "&gid=" + enc(gid);

        SSLContext ctx = SSLContext.getInstance("TLS");
        ctx.init(null, new TrustManager[]{ new WsClient.PinnedTrust(st.fingerprint()) }, new SecureRandom());
        HttpsURLConnection c = (HttpsURLConnection) new URL(url).openConnection();
        c.setSSLSocketFactory(ctx.getSocketFactory());
        c.setHostnameVerifier((h, sess) -> true); // identity is the pinned fingerprint, not the hostname
        c.setRequestMethod(method);
        c.setConnectTimeout(12000);
        c.setReadTimeout(20000);
        if (json != null) {
            c.setDoOutput(true);
            c.setRequestProperty("Content-Type", "application/json");
            try (OutputStream os = c.getOutputStream()) { os.write(json.getBytes("UTF-8")); }
        }
        int code = c.getResponseCode();
        InputStream in = code < 400 ? c.getInputStream() : c.getErrorStream();
        StringBuilder sb = new StringBuilder();
        if (in != null) {
            byte[] buf = new byte[8192]; int n;
            while ((n = in.read(buf)) > 0) sb.append(new String(buf, 0, n, "UTF-8"));
        }
        c.disconnect();
        if (code >= 400) throw new Exception("HTTP " + code + (code == 401 ? " (sign in / check key)" : "") + ": " + sb);
        return sb.toString();
    }

    private static String enc(String s) {
        try { return URLEncoder.encode(s, "UTF-8"); } catch (Exception e) { return s; }
    }
}

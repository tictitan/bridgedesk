package com.bridgedesk.app;

import android.security.keystore.KeyGenParameterSpec;
import android.security.keystore.KeyProperties;
import android.util.Base64;

import java.security.KeyStore;

import javax.crypto.Cipher;
import javax.crypto.KeyGenerator;
import javax.crypto.SecretKey;
import javax.crypto.spec.GCMParameterSpec;

/**
 * Encrypts the Anthropic API key with a hardware-backed AES-GCM key held in the Android Keystore,
 * so only this app (and never plaintext prefs/logs) can read it. We store only ciphertext.
 */
public class SecureKey {
    private static final String KS = "AndroidKeyStore";
    private static final String ALIAS = "aria_api_key";
    private static final String XFORM = "AES/GCM/NoPadding";

    private static SecretKey key() throws Exception {
        KeyStore ks = KeyStore.getInstance(KS);
        ks.load(null);
        if (ks.containsAlias(ALIAS)) {
            return ((KeyStore.SecretKeyEntry) ks.getEntry(ALIAS, null)).getSecretKey();
        }
        KeyGenerator kg = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, KS);
        kg.init(new KeyGenParameterSpec.Builder(ALIAS,
                KeyProperties.PURPOSE_ENCRYPT | KeyProperties.PURPOSE_DECRYPT)
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .build());
        return kg.generateKey();
    }

    /** Returns "ivB64:ctB64" ciphertext, or throws. */
    static String encrypt(String plain) throws Exception {
        Cipher c = Cipher.getInstance(XFORM);
        c.init(Cipher.ENCRYPT_MODE, key());
        byte[] iv = c.getIV();
        byte[] ct = c.doFinal(plain.getBytes("UTF-8"));
        return b64(iv) + ":" + b64(ct);
    }

    /** Decrypts "ivB64:ctB64" back to plaintext, or returns "" on any failure. */
    static String decrypt(String stored) {
        try {
            if (stored == null || !stored.contains(":")) return "";
            String[] p = stored.split(":", 2);
            byte[] iv = unb64(p[0]);
            byte[] ct = unb64(p[1]);
            Cipher c = Cipher.getInstance(XFORM);
            c.init(Cipher.DECRYPT_MODE, key(), new GCMParameterSpec(128, iv));
            return new String(c.doFinal(ct), "UTF-8");
        } catch (Exception e) {
            return "";
        }
    }

    private static String b64(byte[] b) { return Base64.encodeToString(b, Base64.NO_WRAP); }
    private static byte[] unb64(String s) { return Base64.decode(s, Base64.NO_WRAP); }
}

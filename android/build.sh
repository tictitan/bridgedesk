#!/usr/bin/env bash
# Builds a signed BridgeDesk APK with the Android command-line tools only (no Gradle).
set -euo pipefail

SDK="${ANDROID_SDK:-${ANDROID_HOME:-/opt/android-sdk}}"
# Pick the newest installed build-tools (newer d8/R8 is happier on recent JDKs).
BT="$(ls -d "$SDK"/build-tools/*/ 2>/dev/null | sort -V | tail -1)"; BT="${BT%/}"
[ -n "$BT" ] || { echo "No build-tools found under $SDK. Install e.g. build-tools;34.0.0"; exit 1; }
# Pick the newest installed platform android.jar.
PLATFORM="$(ls "$SDK"/platforms/android-*/android.jar 2>/dev/null | sort -V | tail -1)"
[ -n "$PLATFORM" ] || { echo "No platform found. Install e.g. platforms;android-34"; exit 1; }
D8="$BT/d8"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="$HERE/build"
PKG="com.bridgedesk.app"
echo "Using build-tools: $BT"
echo "Using platform:    $PLATFORM"

rm -rf "$OUT"; mkdir -p "$OUT/compiled" "$OUT/gen" "$OUT/classes" "$OUT/apk"

echo "[1/7] compile resources"
"$BT/aapt2" compile --dir "$HERE/res" -o "$OUT/compiled/res.zip"

echo "[2/7] link resources + manifest -> base.apk, R.java"
"$BT/aapt2" link \
  -o "$OUT/apk/base.apk" \
  -I "$PLATFORM" \
  --manifest "$HERE/AndroidManifest.xml" \
  --java "$OUT/gen" \
  --min-sdk-version 26 --target-sdk-version 34 \
  --version-code "${BD_VERSION_CODE:-3}" --version-name "${BD_VERSION_NAME:-1.1.0}" \
  "$OUT/compiled/res.zip"

echo "[3/7] javac"
find "$HERE/src" "$OUT/gen" -name '*.java' > "$OUT/sources.txt"
javac -source 17 -target 17 -encoding UTF-8 \
  -classpath "$PLATFORM" \
  -d "$OUT/classes" \
  @"$OUT/sources.txt" 2> "$OUT/javac.log" || { cat "$OUT/javac.log"; exit 1; }
grep -i warning "$OUT/javac.log" | head -5 || true

echo "[4/7] d8 -> dex"
( cd "$OUT/classes" && jar cf "$OUT/classes.jar" . )
"$D8" --min-api 26 --lib "$PLATFORM" --output "$OUT/apk" "$OUT/classes.jar" 2>"$OUT/d8.log" || { cat "$OUT/d8.log"; exit 1; }

echo "[5/7] package dex + assets into apk"
cd "$OUT/apk"
# base.apk already holds resources + manifest; add classes.dex (and assets if any)
"$BT/aapt2" version >/dev/null
cp base.apk unaligned.apk

# Use zip to inject the dex (stored) — aapt won't add arbitrary files post-link.
( cd "$OUT/apk" && zip -q -j unaligned.apk classes.dex )
if [ -d "$HERE/assets" ] && [ -n "$(ls -A "$HERE/assets" 2>/dev/null)" ]; then
  ( cd "$HERE" && zip -q -r "$OUT/apk/unaligned.apk" assets )
fi

echo "[6/7] zipalign"
"$BT/zipalign" -f -p 4 "$OUT/apk/unaligned.apk" "$OUT/apk/aligned.apk"

echo "[7/7] sign"
# A fixed release key lets new versions install over old ones. CI decodes it from the
# ANDROID_KEYSTORE_B64 secret; locally it is android/release.keystore + android/keystore.pass.
KS="${BD_KEYSTORE:-$HERE/release.keystore}"
PASS="${BD_KS_PASS:-}"
[ -z "$PASS" ] && [ -f "$HERE/keystore.pass" ] && PASS="$(cat "$HERE/keystore.pass")"
if [ ! -f "$KS" ] || [ -z "$PASS" ]; then
  echo "  (no release key — signing with a throwaway debug key; updates won't install over other builds)"
  KS="$HERE/debug.keystore"; PASS=bridgedesk
  if [ ! -f "$KS" ]; then
    keytool -genkeypair -v -keystore "$KS" -storepass bridgedesk -keypass bridgedesk \
      -alias bridgedesk -keyalg RSA -keysize 2048 -validity 10000 \
      -dname "CN=BridgeDesk, OU=Dev, O=BridgeDesk, L=, S=, C=US" >/dev/null 2>&1
  fi
fi
"$BT/apksigner" sign --ks "$KS" --ks-pass "pass:$PASS" --key-pass "pass:$PASS" \
  --min-sdk-version 26 --v1-signing-enabled true --v2-signing-enabled true \
  --out "$HERE/BridgeDesk.apk" "$OUT/apk/aligned.apk"
"$BT/apksigner" verify --print-certs "$HERE/BridgeDesk.apk" >/dev/null && echo "signature OK"

echo "APK: $HERE/BridgeDesk.apk"
ls -la "$HERE/BridgeDesk.apk"

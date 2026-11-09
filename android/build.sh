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
# Resolve a build-tool to its real file: bare on Linux/macOS, .bat/.exe on Windows (Git Bash).
bt() { for e in "" ".bat" ".exe" ".cmd"; do [ -f "$BT/$1$e" ] && { echo "$BT/$1$e"; return; }; done; echo "$BT/$1"; }
AAPT2="$(bt aapt2)"; D8="$(bt d8)"; ZIPALIGN="$(bt zipalign)"; APKSIGNER="$(bt apksigner)"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="$HERE/build"
PKG="com.bridgedesk.app"
echo "Using build-tools: $BT"
echo "Using platform:    $PLATFORM"

rm -rf "$OUT"; mkdir -p "$OUT/compiled" "$OUT/gen" "$OUT/classes" "$OUT/apk"

echo "[1/7] compile resources"
"$AAPT2" compile --dir "$HERE/res" -o "$OUT/compiled/res.zip"

echo "[2/7] link resources + manifest -> base.apk, R.java"
"$AAPT2" link \
  -o "$OUT/apk/base.apk" \
  -I "$PLATFORM" \
  --manifest "$HERE/AndroidManifest.xml" \
  --java "$OUT/gen" \
  --min-sdk-version 26 --target-sdk-version 34 \
  --version-code "${BD_VERSION_CODE:-3}" --version-name "${BD_VERSION_NAME:-1.1.0}" \
  "$OUT/compiled/res.zip"

echo "[3/7] javac"
# On Git Bash / MSYS (Windows) the JDK's javac can't read /c/... style paths, so translate
# the classpath, output dir and source list to native form. A no-op on Linux/macOS (CI).
if command -v cygpath >/dev/null 2>&1; then
  JAVAC_CP="$(cygpath -w "$PLATFORM")"; JAVAC_OUT="$(cygpath -w "$OUT/classes")"
  find "$HERE/src" "$OUT/gen" -name '*.java' | cygpath -w -f - > "$OUT/sources.txt"
else
  JAVAC_CP="$PLATFORM"; JAVAC_OUT="$OUT/classes"
  find "$HERE/src" "$OUT/gen" -name '*.java' > "$OUT/sources.txt"
fi
javac -source 17 -target 17 -encoding UTF-8 \
  -classpath "$JAVAC_CP" \
  -d "$JAVAC_OUT" \
  @"$OUT/sources.txt" 2> "$OUT/javac.log" || { cat "$OUT/javac.log"; exit 1; }
grep -i warning "$OUT/javac.log" | head -5 || true

echo "[4/7] d8 -> dex"
( cd "$OUT/classes" && jar cf "$OUT/classes.jar" . )
"$D8" --min-api 26 --lib "$PLATFORM" --output "$OUT/apk" "$OUT/classes.jar" 2>"$OUT/d8.log" || { cat "$OUT/d8.log"; exit 1; }

echo "[5/7] package dex + assets into apk"
cd "$OUT/apk"
# base.apk already holds resources + manifest; add classes.dex (and assets if any)
"$AAPT2" version >/dev/null
cp base.apk unaligned.apk

# Inject classes.dex (and assets) into the apk — aapt won't add arbitrary files post-link.
# Prefer `zip` (present on Linux/CI); fall back to Python's zipfile on Windows Git Bash,
# which ships no `zip`. zipalign runs next, so stored-vs-deflated here doesn't matter.
ASSETS=""
if [ -d "$HERE/assets" ] && [ -n "$(ls -A "$HERE/assets" 2>/dev/null)" ]; then ASSETS="$HERE/assets"; fi
if command -v zip >/dev/null 2>&1; then
  ( cd "$OUT/apk" && zip -q -j unaligned.apk classes.dex )
  [ -n "$ASSETS" ] && ( cd "$HERE" && zip -q -r "$OUT/apk/unaligned.apk" assets )
else
  PY="$(command -v python || command -v python3)"
  [ -n "$PY" ] || { echo "Need either 'zip' or Python to package the apk"; exit 1; }
  APK="$OUT/apk/unaligned.apk" DEX="$OUT/apk/classes.dex" ASSETS_DIR="$ASSETS" "$PY" - <<'PY'
import os, zipfile
apk = os.environ["APK"]; dex = os.environ["DEX"]; assets = os.environ.get("ASSETS_DIR","")
with zipfile.ZipFile(apk, "a", zipfile.ZIP_DEFLATED) as z:
    z.write(dex, "classes.dex")
    if assets:
        base = os.path.dirname(assets)
        for root, _, files in os.walk(assets):
            for f in files:
                full = os.path.join(root, f)
                z.write(full, os.path.relpath(full, base).replace(os.sep, "/"))
print("injected classes.dex" + (" + assets" if assets else ""))
PY
fi

echo "[6/7] zipalign"
"$ZIPALIGN" -f -p 4 "$OUT/apk/unaligned.apk" "$OUT/apk/aligned.apk"

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
"$APKSIGNER" sign --ks "$KS" --ks-pass "pass:$PASS" --key-pass "pass:$PASS" \
  --min-sdk-version 26 --v1-signing-enabled true --v2-signing-enabled true \
  --out "$HERE/BridgeDesk.apk" "$OUT/apk/aligned.apk"
"$APKSIGNER" verify --print-certs "$HERE/BridgeDesk.apk" >/dev/null && echo "signature OK"

echo "APK: $HERE/BridgeDesk.apk"
ls -la "$HERE/BridgeDesk.apk"

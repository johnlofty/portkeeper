#!/bin/bash
# Assembles build/Portkeeper.app: the SwiftUI menu-bar app, the Go daemon beside it in
# Contents/MacOS, and the launchd plist SMAppService registers it with. Ad-hoc signed.
#
# Builds and signs only. It never registers, loads or launches anything; that is a
# decision made in the app's Settings, from the copy of the bundle that is going to stay.
set -euo pipefail
cd "$(dirname "$0")/.."

ROOT=$(pwd)
APP="$ROOT/build/Portkeeper.app"
PKG="$ROOT/macos/Portkeeper"
# The version comes in from `make dist VERSION=<tag>` (a v* tag in CI) and defaults to a
# dev marker. CFBundleShortVersionString and CFBundleVersion want dotted numerics, so the
# leading "v" is dropped and anything that is not a version number reports 0.0.0; the zip
# name still carries the raw string.
VERSION="${VERSION:-dev}"
SHORT="${VERSION#v}"
[[ "$SHORT" =~ ^[0-9]+(\.[0-9]+){1,2}$ ]] || SHORT="0.0.0"

# XCTest and xcodebuild live in Xcode, not the Command Line Tools. If xcode-select still
# points at the CLT, use Xcode for this build without changing the machine's setting.
if [[ -z "${DEVELOPER_DIR:-}" && "$(xcode-select -p 2>/dev/null)" == /Library/Developer/CommandLineTools* \
      && -d /Applications/Xcode.app/Contents/Developer ]]; then
    export DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer
fi

echo "==> go build"
go build -o bin/portkeeperd ./cmd/portkeeperd

echo "==> swift build -c release"
(cd "$PKG" && swift build -c release)
BIN=$(cd "$PKG" && swift build -c release --show-bin-path)

echo "==> assemble $APP"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources" "$APP/Contents/Library/LaunchAgents"
cp "$BIN/Portkeeper" "$APP/Contents/MacOS/Portkeeper"
cp bin/portkeeperd "$APP/Contents/MacOS/portkeeperd"

ICON_KEY=""
ICONSET="$ROOT/build/AppIcon.iconset"
rm -rf "$ICONSET"
if swift macos/make-icon.swift "$ICONSET" && iconutil -c icns -o "$APP/Contents/Resources/AppIcon.icns" "$ICONSET"; then
    ICON_KEY="	<key>CFBundleIconFile</key>
	<string>AppIcon</string>"
else
    echo "warning: icon not generated; the bundle will use the generic icon" >&2
fi
rm -rf "$ICONSET"

# The bundle id is distinct from both launchd labels: an app and a job are different
# things to macOS, and sharing a name between them only confuses the Login Items list.
cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>io.github.johnlofty.portkeeper.app</string>
	<key>CFBundleName</key>
	<string>Portkeeper</string>
	<key>CFBundleDisplayName</key>
	<string>Portkeeper</string>
	<key>CFBundleExecutable</key>
	<string>Portkeeper</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>$SHORT</string>
	<key>CFBundleVersion</key>
	<string>1</string>
	<key>CFBundleInfoDictionaryVersion</key>
	<string>6.0</string>
$ICON_KEY
	<key>LSMinimumSystemVersion</key>
	<string>14.0</string>
	<key>LSUIElement</key>
	<true/>
	<key>NSHighResolutionCapable</key>
	<true/>
	<!-- The daemon is plain http on loopback. -->
	<key>NSAppTransportSecurity</key>
	<dict>
		<key>NSAllowsLocalNetworking</key>
		<true/>
	</dict>
</dict>
</plist>
PLIST

# BundleProgram, not ProgramArguments: SMAppService resolves it against wherever the
# bundle is when the job is registered, which is why the app must be registered from the
# copy that stays. Otherwise this is the dev plist's job, under its own label.
cat > "$APP/Contents/Library/LaunchAgents/io.github.johnlofty.portkeeper.helper.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>io.github.johnlofty.portkeeper.helper</string>
	<key>BundleProgram</key>
	<string>Contents/MacOS/portkeeperd</string>
	<key>RunAtLoad</key>
	<true/>
	<!-- Restart on a crash, not on a clean exit. The daemon exits non-zero when the
	     listen port is taken, so a second daemon would be restarted into the same
	     failure; the throttle keeps that to one attempt every ten seconds. -->
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>LimitLoadToSessionType</key>
	<string>Aqua</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin</string>
	</dict>
	<key>StandardOutPath</key>
	<string>/tmp/portkeeper.log</string>
	<key>StandardErrorPath</key>
	<string>/tmp/portkeeper.log</string>
</dict>
</plist>
PLIST

echo "==> plutil -lint"
plutil -lint "$APP/Contents/Info.plist" "$APP/Contents/Library/LaunchAgents/io.github.johnlofty.portkeeper.helper.plist"

# The daemon is signed on its own first: --deep does not reliably reach a second
# executable sitting loose in Contents/MacOS.
echo "==> codesign (ad-hoc)"
codesign --force -s - "$APP/Contents/MacOS/portkeeperd"
codesign --force --deep -s - "$APP"

echo "built: $APP"

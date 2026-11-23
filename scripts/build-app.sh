#!/bin/sh
set -eu

cd "$(dirname "$0")/.."
source_dir=app
output_dir="$source_dir/build"
mkdir -p "$output_dir"
build_dir=$(mktemp -d "$output_dir/.build.XXXXXXXX")
trap 'rm -rf -- "$build_dir"' EXIT
app="$build_dir/OCM.app"
mkdir -p "$app/Contents/MacOS"
cp "$source_dir/Info.plist" "$app/Contents/Info.plist"
xcrun clang -arch arm64 -arch x86_64 -mmacosx-version-min=11.0 \
  -fobjc-arc -framework AppKit -framework CoreServices -framework UserNotifications \
  "$source_dir/main.m" "$source_dir/Notifications.m" -o "$app/Contents/MacOS/notifier"
lipo "$app/Contents/MacOS/notifier" -verify_arch arm64 x86_64
codesign --force --sign - "$app"
codesign --verify --strict "$app"
python3 - "$app" "internal/desktopnotify/assets/notifier.zip" <<'ZIP'
import sys
from pathlib import Path
from zipfile import ZIP_DEFLATED, ZipFile

app, destination = map(Path, sys.argv[1:])
temporary = destination.with_suffix(".tmp")
with ZipFile(temporary, "w", ZIP_DEFLATED) as archive:
    for entry in ("Contents/Info.plist", "Contents/MacOS/notifier", "Contents/_CodeSignature/CodeResources"):
        archive.write(app / entry, entry)
temporary.replace(destination)
ZIP
output="$output_dir/OCM.app"
rm -rf -- "$output"
mv "$app" "$output"
printf '%s\n' "$output"

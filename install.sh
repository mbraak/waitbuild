#!/usr/bin/env bash
# Builds waitbuild, waitbuild-menubar and waitbuild-tui and installs them into
# $BIN (default ~/.local/bin). When the menu bar app runs as a LaunchAgent, it
# is restarted so that it uses the new binary.
#
# Usage: ./install.sh
set -euo pipefail

bin=${BIN:-$HOME/.local/bin}
cd "$(dirname "$0")"
mkdir -p "$bin"

for cmd in waitbuild waitbuild-menubar waitbuild-tui; do
  pkg=./cmd/$cmd
  [ "$cmd" = waitbuild ] && pkg=.
  # Build next to the target first, so that a failed build leaves the
  # installed binary alone. Then delete the old binary before moving the new
  # one in: macOS caches the code signature of a file, and refuses to start a
  # binary that was overwritten in place.
  go build -o "$bin/.$cmd.new" "$pkg"
  rm -f "$bin/$cmd"
  mv "$bin/.$cmd.new" "$bin/$cmd"
  echo "installed $bin/$cmd"
done

domain=gui/$(id -u)
label=com.github.mbraak.waitbuild-menubar
service=$domain/$label
plist=$HOME/Library/LaunchAgents/$label.plist
if launchctl print "$service" >/dev/null 2>&1; then
  # Unload and load the agent instead of kickstarting it: launchd pins the
  # code signature (cdhash) of the binary it registered, and refuses to spawn
  # a rebuilt one (OS_REASON_CODESIGNING) until the agent is registered again.
  # bootout returns before the agent is gone, and bootstrapping it before
  # then fails with "Input/output error", so wait for it.
  launchctl bootout "$service" 2>/dev/null || true
  for _ in $(seq 50); do
    launchctl print "$service" >/dev/null 2>&1 || break
    sleep 0.1
  done
  launchctl bootstrap "$domain" "$plist"
  echo "restarted the waitbuild-menubar LaunchAgent"
elif [ -f "$plist" ] && grep -q "<string>$bin/waitbuild-menubar</string>" "$plist"; then
  launchctl bootstrap "$domain" "$plist"
  echo "started the waitbuild-menubar LaunchAgent"
elif pgrep -xq waitbuild-menubar; then
  echo "waitbuild-menubar is running; restart it to use the new binary"
fi

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

service=gui/$(id -u)/com.github.mbraak.waitbuild-menubar
program=$(launchctl print "$service" 2>/dev/null | sed -n 's/^[[:space:]]*program = //p' | head -n 1)
if [ "$program" = "$bin/waitbuild-menubar" ]; then
  launchctl kickstart -k "$service"
  echo "restarted the waitbuild-menubar LaunchAgent"
elif pgrep -xq waitbuild-menubar; then
  echo "waitbuild-menubar is running; restart it to use the new binary"
fi

#!/bin/sh
# Run pr-jump at login on macOS.
#   ./install-macos.sh --owner my-org [--owner another] [--port 7777]
#   ./install-macos.sh --uninstall
set -eu

label=local.pr-jump
plist="$HOME/Library/LaunchAgents/$label.plist"
data="${XDG_DATA_HOME:-$HOME/.local/share}/pr-jump"

if [ "${1:-}" = --uninstall ]; then
  launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
  rm -f "$plist"
  echo "Removed $label. The index is still in $data."
  exit 0
fi
if [ $# -eq 0 ]; then
  echo "usage: $0 --owner <user-or-org> [--owner ...] [--port 7777]" >&2
  echo "       $0 --uninstall" >&2
  exit 2
fi

script="$(cd "$(dirname "$0")" && pwd)/pr_jump.py"
python=$(command -v python3)
"$python" -c 'import sys; assert sys.version_info >= (3, 9)' 2>/dev/null || { echo "pr-jump needs Python 3.9 or later" >&2; exit 1; }
# The launch agent gets its token from gh, because it cannot see GITHUB_TOKEN from your shell.
gh=$(command -v gh) || { echo "Install the GitHub CLI (gh) and run 'gh auth login' first" >&2; exit 1; }
"$python" "$script" "$@" --data-dir "$data" --validate
mkdir -p "$data" && chmod 700 "$data"
mkdir -p "$(dirname "$plist")"

xml() { printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }
args=""
for arg in "$python" "$script" "$@" --data-dir "$data"; do args="$args<string>$(xml "$arg")</string>"; done

cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array>$args</array>
  <key>EnvironmentVariables</key><dict><key>PATH</key><string>$(xml "$(dirname "$gh")"):/usr/bin:/bin</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>$(xml "$data/server.log")</string>
</dict>
</plist>
EOF
plutil -lint "$plist" >/dev/null
launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$plist"
echo "Installed $label. The first index takes a minute or two; progress goes to $data/server.log."

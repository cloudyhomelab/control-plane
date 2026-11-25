#!/bin/sh
set -e

# deb passes an action, rpm the count of versions left (0 on erase), Arch the old version
# and only on removal. Only an upgrade keeps the service running.
case "$1" in
  upgrade | failed-upgrade | deconfigure | [1-9] | [1-9][0-9]) exit 0 ;;
esac

if [ -d /run/systemd/system ]; then
  systemctl disable --now controlplane.service || true
fi

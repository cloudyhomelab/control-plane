#!/bin/sh
set -e

# Not every distro runs sysusers.d on package install (Arch does, Debian and RPM may not).
systemd-sysusers controlplane.conf

if [ -d /run/systemd/system ]; then
  systemctl daemon-reload
fi

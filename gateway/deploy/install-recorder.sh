#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo "Run this installer as root." >&2
  exit 1
fi

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_DIR=$(dirname "$SCRIPT_DIR")
ARCHIVE="$PROJECT_DIR/mediamtx_v1.20.0_linux_armv7.tar.gz"
EXPECTED_SHA256="12f1f6fae3aad0153580b265bfcceb30b44194adbe92172f3c0d6a27fbeecf98"
RECORDING_UUID="B4B26378B2633DCC"
MOUNT_POINT="/mnt/recordings"

if [ ! -f "$ARCHIVE" ]; then
  echo "Missing $ARCHIVE" >&2
  exit 1
fi
printf '%s  %s\n' "$EXPECTED_SHA256" "$ARCHIVE" | sha256sum --check --status || {
  echo "MediaMTX archive checksum mismatch." >&2
  exit 1
}

if [ ! -e "/dev/disk/by-uuid/$RECORDING_UUID" ]; then
  echo "Recording disk UUID $RECORDING_UUID is not connected." >&2
  exit 1
fi

mkdir -p "$MOUNT_POINT"
if ! grep -q "UUID=$RECORDING_UUID[[:space:]]" /etc/fstab; then
  cp -a /etc/fstab "/etc/fstab.bak-lens-recorder-$(date +%Y%m%d%H%M%S)"
  printf '%s\n' "UUID=$RECORDING_UUID $MOUNT_POINT ntfs3 rw,noatime,uid=999,gid=995,umask=0027,nofail,x-systemd.device-timeout=10s 0 0" >> /etc/fstab
fi
mountpoint -q "$MOUNT_POINT" || mount "$MOUNT_POINT"
sudo -u lens-gateway test -w "$MOUNT_POINT" || {
  echo "$MOUNT_POINT is not writable by lens-gateway." >&2
  exit 1
}

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT
tar -xzf "$ARCHIVE" -C "$tmp_dir" mediamtx
install -m 0755 "$tmp_dir/mediamtx" /usr/local/bin/mediamtx

if [ -f "$PROJECT_DIR/lens-gateway-armv7" ]; then
  install -m 0755 "$PROJECT_DIR/lens-gateway-armv7" /usr/local/bin/lens-gateway
fi

install -d -m 0750 -o lens-gateway -g lens-gateway /etc/lens-gateway
install -m 0640 -o lens-gateway -g lens-gateway "$SCRIPT_DIR/mediamtx.yml" /etc/lens-gateway/mediamtx.yml
install -d -m 0755 /usr/local/lib/lens-gateway
install -m 0755 "$SCRIPT_DIR/recording-space-guard.sh" /usr/local/lib/lens-gateway/recording-space-guard.sh
install -m 0644 "$SCRIPT_DIR/mediamtx.service" /etc/systemd/system/mediamtx.service
install -m 0644 "$SCRIPT_DIR/recording-space-guard.service" /etc/systemd/system/recording-space-guard.service
install -m 0644 "$SCRIPT_DIR/recording-space-guard.timer" /etc/systemd/system/recording-space-guard.timer

if grep -q '^  listen: ""$' /etc/lens-gateway/go2rtc.yaml; then
  sed -i '0,/^  listen: ""$/s//  listen: "127.0.0.1:8554"/' /etc/lens-gateway/go2rtc.yaml
fi
grep -q '^  listen: "127.0.0.1:8554"$' /etc/lens-gateway/go2rtc.yaml || {
  echo "Unable to configure the loopback go2rtc RTSP listener." >&2
  exit 1
}
if ! grep -q '^  default_query: "video"$' /etc/lens-gateway/go2rtc.yaml; then
  sed -i '/^  listen: "127.0.0.1:8554"$/a\  default_query: "video"' /etc/lens-gateway/go2rtc.yaml
fi

systemctl daemon-reload
systemctl enable go2rtc mediamtx recording-space-guard.timer
systemctl restart go2rtc
systemctl restart mediamtx
if [ -f "$PROJECT_DIR/lens-gateway-armv7" ]; then
  systemctl restart lens-gateway
fi
systemctl start recording-space-guard.timer

echo "Recorder installed. Verify with:"
echo "  systemctl status lens-gateway mediamtx recording-space-guard.timer --no-pager"
echo "  journalctl -u mediamtx -n 50 --no-pager"
echo "  find $MOUNT_POINT -type f | head"

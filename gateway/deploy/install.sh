#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo "Run this installer as root." >&2
  exit 1
fi

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
GO2RTC_VERSION=${GO2RTC_VERSION:-1.9.14}
GO2RTC_SHA256=${GO2RTC_SHA256:-4d7e1639af5a2722a28e864468fd8099b3c1682565446c798bf9e3b38fde12e4}
GO2RTC_URL="https://github.com/AlexxIT/go2rtc/releases/download/v${GO2RTC_VERSION}/go2rtc_linux_arm"

verify_go2rtc() {
  printf '%s  %s\n' "$GO2RTC_SHA256" "$1" | sha256sum -c - >/dev/null 2>&1
}

download_go2rtc() {
  output=$1
  attempt=1
  while [ "$attempt" -le 5 ]; do
    echo "Downloading go2rtc over HTTP/1.1 (attempt $attempt/5)..."
    if curl --fail --location --http1.1 --connect-timeout 20 --max-time 300 \
      --continue-at - --output "$output" "$GO2RTC_URL"; then
      if verify_go2rtc "$output"; then
        return 0
      fi
      echo "Downloaded go2rtc failed SHA-256 verification; restarting download." >&2
      : > "$output"
    fi
    attempt=$((attempt + 1))
    sleep 2
  done

  if command -v wget >/dev/null 2>&1; then
    echo "curl could not finish the download; trying wget..."
    rm -f "$output"
    if wget --tries=5 --timeout=30 --no-http-keep-alive -O "$output" "$GO2RTC_URL" &&
      verify_go2rtc "$output"; then
      return 0
    fi
  fi
  return 1
}

if [ ! -f "$PROJECT_DIR/lens-gateway-armv7" ]; then
  echo "Missing gateway/lens-gateway-armv7. Run deploy/build-armv7.ps1 first." >&2
  exit 1
fi

if ! id lens-gateway >/dev/null 2>&1; then
  useradd --system --home /var/lib/lens-gateway --create-home --shell /usr/sbin/nologin lens-gateway
fi

install -m 0755 "$PROJECT_DIR/lens-gateway-armv7" /usr/local/bin/lens-gateway
tmp_go2rtc=$(mktemp)
trap 'rm -f "$tmp_go2rtc"' EXIT

offline_go2rtc=""
for candidate in "$PROJECT_DIR/go2rtc_linux_arm" "$SCRIPT_DIR/go2rtc_linux_arm"; do
  if [ -f "$candidate" ] && verify_go2rtc "$candidate"; then
    offline_go2rtc=$candidate
    break
  fi
done

if [ -n "$offline_go2rtc" ]; then
  echo "Using verified offline go2rtc binary: $offline_go2rtc"
  cp "$offline_go2rtc" "$tmp_go2rtc"
elif ! download_go2rtc "$tmp_go2rtc"; then
  echo "Unable to download a verified go2rtc binary." >&2
  echo "Download go2rtc_linux_arm v${GO2RTC_VERSION} on another computer," >&2
  echo "place it in the gateway directory, then rerun this installer." >&2
  exit 1
fi

if ! verify_go2rtc "$tmp_go2rtc"; then
  echo "go2rtc SHA-256 verification failed; refusing to install it." >&2
  exit 1
fi
install -m 0755 "$tmp_go2rtc" /usr/local/bin/go2rtc

install -d -m 0750 -o lens-gateway -g lens-gateway /etc/lens-gateway
if [ ! -f /etc/lens-gateway/config.json ]; then
  install -m 0640 -o lens-gateway -g lens-gateway "$PROJECT_DIR/config.example.json" /etc/lens-gateway/config.json
fi
if [ ! -f /etc/lens-gateway/go2rtc.yaml ]; then
  install -m 0640 -o lens-gateway -g lens-gateway "$SCRIPT_DIR/go2rtc.yaml.example" /etc/lens-gateway/go2rtc.yaml
fi
if [ ! -f /etc/lens-gateway/secrets.env ]; then
  install -m 0600 -o lens-gateway -g lens-gateway "$SCRIPT_DIR/secrets.env.example" /etc/lens-gateway/secrets.env
fi

install -m 0644 "$SCRIPT_DIR/lens-gateway.service" /etc/systemd/system/lens-gateway.service
install -m 0644 "$SCRIPT_DIR/go2rtc.service" /etc/systemd/system/go2rtc.service
systemctl daemon-reload

echo "Installed. Fill /etc/lens-gateway/secrets.env, then run:"
echo "  systemctl enable --now go2rtc lens-gateway"
echo "  tailscale serve --https=443 http://127.0.0.1:80"

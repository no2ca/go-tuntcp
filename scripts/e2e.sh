#!/usr/bin/env bash
# ビルド → 起動 → IP 割り当て → curl → 終了 までを 1 つのターミナルで行う。
set -euo pipefail

TUN_NAME="${TUN_NAME:-tun0}"
TUN_PEER_IP="${TUN_PEER_IP:-10.10.0.2}"
BIN=./go-tuntcp

go build -o "$BIN" ./cmd/tuntcp

# TUN デバイスの作成に必要な権限だけをバイナリに付与する (sudo で起動しないため)
sudo setcap cap_net_admin+ep "$BIN"

"$BIN" &
pid=$!
trap 'kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true' EXIT

# TUN デバイスができるまで待つ。先にプロセスが落ちたら中断する
until ip link show "$TUN_NAME" &>/dev/null; do
  if ! kill -0 "$pid" 2>/dev/null; then
    echo "e2e: $BIN が起動に失敗しました" >&2
    exit 1
  fi
  sleep 0.1
done

task setup-interface

curl -m 5 "$TUN_PEER_IP" || true

#!/usr/bin/env bash
# ビルド → 起動 → IP 割り当て → curl → 終了 までを 1 つのターミナルで行う。
#
# sudo は使わない。unshare で作ったユーザー名前空間とネットワーク名前空間の中で動かす。
# 名前空間の中では CAP_NET_ADMIN を持つので、TUN デバイスの作成と IP の割り当てができる。
# tun0 は名前空間の中にだけ存在し、スクリプトが終わると名前空間ごと消える。
set -euo pipefail

TUN_NAME="${TUN_NAME:-tun0}"
TUN_IP="${TUN_IP:-10.10.0.1}"
TUN_PEER_IP="${TUN_PEER_IP:-10.10.0.2}"
BIN=./go-tuntcp

if [[ -z "${E2E_IN_NS:-}" ]]; then
  # ビルドは名前空間の外で行う (名前空間の中にはネットワークがない)
  go build -o "$BIN" ./cmd/tuntcp
  E2E_IN_NS=1 exec unshare --user --map-root-user --net "$0" "$@"
fi

ip link set lo up

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

ip addr add "$TUN_IP" peer "$TUN_PEER_IP" dev "$TUN_NAME"
ip link set "$TUN_NAME" up

curl -m 5 "$TUN_PEER_IP" || true

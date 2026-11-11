#!/usr/bin/env bash
# ビルド → 起動 → IP 割り当て → curl → 終了 までを 1 つのターミナルで行う。
# 流れたパケットを tcpdump で記録し、tmp/e2e/ に残す。
#
# sudo は使わない。unshare で作ったユーザー名前空間とネットワーク名前空間の中で動かす。
# 名前空間の中では CAP_NET_ADMIN を持つので、TUN デバイスの作成と IP の割り当てができる。
# tun0 は名前空間の中にだけ存在し、スクリプトが終わると名前空間ごと消える。
set -euo pipefail

TUN_NAME="${TUN_NAME:-tun0}"
TUN_IP="${TUN_IP:-10.10.0.1}"
TUN_PEER_IP="${TUN_PEER_IP:-10.10.0.2}"
BIN=./go-tuntcp
OUT=tmp/e2e

if [[ -z "${E2E_IN_NS:-}" ]]; then
  # ビルドは名前空間の外で行う (名前空間の中にはネットワークがない)
  go build -o "$BIN" ./cmd/tuntcp
  E2E_IN_NS=1 exec unshare --user --map-root-user --net "$0" "$@"
fi

mkdir -p "$OUT"
# 前回の記録を消す。残っていると、下の「tcpdump がキャプチャを始めるまで待つ」が
# 前回の tcpdump.err を見てすぐ抜けてしまい、handshake を取りこぼす
rm -f "$OUT"/tcpdump.txt "$OUT"/tcpdump.err "$OUT"/stack.log "$OUT"/ss.txt
ip link set lo up

pids=()
cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap cleanup EXIT

# 自作スタックの出力 (16 進ダンプなど) はファイルに残し、画面には tcpdump の結果を出す
"$BIN" >"$OUT/stack.log" 2>&1 &
pid=$!
pids+=("$pid")

# TUN デバイスができるまで待つ。先にプロセスが落ちたら中断する
until ip link show "$TUN_NAME" &>/dev/null; do
  if ! kill -0 "$pid" 2>/dev/null; then
    echo "e2e: $BIN が起動に失敗しました ($OUT/stack.log を参照)" >&2
    exit 1
  fi
  sleep 0.1
done

ip addr add "$TUN_IP" peer "$TUN_PEER_IP" dev "$TUN_NAME"
ip link set "$TUN_NAME" up

# 実際に流れたパケットを tcpdump で記録する。1 セグメントが 1 行になる。
# 名前空間の中では権限を落とす先のユーザーがいないが、-Z を付けなくても動く
tcpdump -i "$TUN_NAME" -n -l -ttt tcp >"$OUT/tcpdump.txt" 2>"$OUT/tcpdump.err" &
dump=$!
pids+=("$dump")

# tcpdump がキャプチャを始めるまで待つ
until grep -q 'listening on' "$OUT/tcpdump.err"; do
  if ! kill -0 "$dump" 2>/dev/null; then
    echo "e2e: tcpdump が起動に失敗しました" >&2
    cat "$OUT/tcpdump.err" >&2
    exit 1
  fi
  sleep 0.1
done

# curl を裏で走らせ、その最中のカーネル側のソケットの状態を ss で記録する。
# -t は TCP だけ、-n は名前を引かずに番号で表示する。
# handshake は 1 ミリ秒ほどで終わるが、curl の起動を待つ必要があるので、
# ESTAB が出るまで 0.1 秒おきに最大 3 秒見る。出なければ、最後に見た状態が残る
curl -sS -m 5 "$TUN_PEER_IP" &
curl_pid=$!
for _ in $(seq 30); do
  ss -tn >"$OUT/ss.txt"
  if grep -q '^ESTAB' "$OUT/ss.txt"; then break; fi
  sleep 0.1
done
wait "$curl_pid" || true

# tcpdump を止めて出力を書き切らせてから表示する
kill -INT "$dump" 2>/dev/null || true
wait "$dump" 2>/dev/null || true

echo "--- tcpdump ($OUT/tcpdump.txt) ---"
cat "$OUT/tcpdump.txt"
echo "--- ss -tn ($OUT/ss.txt) ---"
cat "$OUT/ss.txt"

./scripts/e2e-check.sh "$OUT"

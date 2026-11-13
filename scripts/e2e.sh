#!/usr/bin/env bash
# ビルド → 起動 → IP 割り当て → クライアント 2 本 (curl、つないで閉じるだけ) → 終了 までを 1 つのターミナルで行う。
# 流れたパケットを tcpdump で、クライアントが終わったあとのソケットの状態を ss で記録し、tmp/e2e/ に残す。
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
rm -f "$OUT"/tcpdump*.txt "$OUT"/tcpdump.err "$OUT"/stack.log "$OUT"/ss*.txt "$OUT"/curl.* "$OUT"/client.*
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
# -Z root は、起動後に権限を落とさせない。tcpdump を root で起動すると、パッケージによっては
# 専用のユーザー (tcpdump) に切り替えようとするが、名前空間の中に対応付けてあるのは自分 (root) だけなので、
# 切り替えに失敗して終わる環境がある ("Couldn't change to 'tcpdump' uid=...")。
# --immediate-mode は、カーネルから届いたパケットを溜めずにすぐ渡させる。付けないと、
# curl が数ミリ秒で終わったとき、溜まっていたパケットを書き出す前に下で止めてしまい、記録が空になる
dump=
start_dump() { # $1: 記録先
  : >"$OUT/tcpdump.err"
  tcpdump -Z root -i "$TUN_NAME" -n -l -ttt --immediate-mode tcp >"$1" 2>"$OUT/tcpdump.err" &
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
}
# tcpdump を止めて、出力を書き切らせる
stop_dump() {
  kill -INT "$dump" 2>/dev/null || true
  wait "$dump" 2>/dev/null || true
}

# クライアントが終わったあとの、カーネル側のソケットの状態を ss で記録する。
# -t は TCP だけ、-n は名前を引かずに番号で表示する。-a を付けないので、LISTEN と TIME-WAIT は出ない
# (カーネルが先に閉じたときは、カーネル側が TIME-WAIT に 60 秒留まる。これは正しい後始末なので、残りに数えない)。
# 最後の ACK のやりとりはクライアントが終わったあとに起きるので、自作スタック宛ての接続が
# 消えるまで 0.1 秒おきに最大 3 秒待つ。消えなければ、最後に見た状態 (FIN-WAIT-2 など) が残る
record_ss() { # $1: 記録先
  for _ in $(seq 30); do
    ss -tn >"$1"
    if ! grep -q "$TUN_PEER_IP:" "$1"; then break; fi
    sleep 0.1
  done
}

# --- 1 本目: curl。自作スタックがレスポンスのあとに先に閉じる ---
# 受け取った本文は curl.txt、エラーは curl.err、終了コードは curl.exit に残す
start_dump "$OUT/tcpdump.txt"
curl_exit=0
curl -sS -m 5 -o "$OUT/curl.txt" "$TUN_PEER_IP" 2>"$OUT/curl.err" || curl_exit=$?
echo "$curl_exit" >"$OUT/curl.exit"
record_ss "$OUT/ss.txt"
stop_dump

# --- 2 本目: つないで、何も送らずに閉じる。カーネルが先に閉じる ---
# bash の /dev/tcp で接続し、すぐ閉じる。自作スタックは、相手の FIN を受けてから閉じる側になる
start_dump "$OUT/tcpdump-close.txt"
client_exit=0
(exec 3<>"/dev/tcp/$TUN_PEER_IP/80" && exec 3>&-) 2>"$OUT/client.err" || client_exit=$?
echo "$client_exit" >"$OUT/client.exit"
record_ss "$OUT/ss-close.txt"
stop_dump

echo "--- 1 本目: tcpdump ($OUT/tcpdump.txt) ---"
cat "$OUT/tcpdump.txt"
echo "--- 1 本目: curl のあとの ss -tn ($OUT/ss.txt) ---"
cat "$OUT/ss.txt"
echo "--- 1 本目: curl (終了コード $curl_exit、本文 $OUT/curl.txt) ---"
cat "$OUT/curl.txt" 2>/dev/null || true
cat "$OUT/curl.err"
echo "--- 2 本目: tcpdump ($OUT/tcpdump-close.txt) ---"
cat "$OUT/tcpdump-close.txt"
echo "--- 2 本目: クライアントのあとの ss -tn ($OUT/ss-close.txt) ---"
cat "$OUT/ss-close.txt"
cat "$OUT/client.err"

./scripts/e2e-check.sh "$OUT"

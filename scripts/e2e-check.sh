#!/usr/bin/env bash
# task e2e が残した記録 (tmp/e2e/) から、期待する振る舞いを確かめる。
# 1 つでも満たさなければ 0 以外で終わる。
#
# 記録を取る側 (e2e.sh) と判定する側を分けてある。記録だけを差し替えて判定を試せる。
# マイルストーンが進んだら、ここに確認を足す。
set -euo pipefail

OUT="${1:-tmp/e2e}"
CLIENT="${TUN_IP:-10.10.0.1}"      # カーネル (curl) 側
SERVER="${TUN_PEER_IP:-10.10.0.2}" # 自作スタック側
PORT="${PORT:-80}"
DUMP="$OUT/tcpdump.txt"

if [[ ! -s "$DUMP" ]]; then
  echo "e2e-check: $DUMP が空か、存在しません" >&2
  exit 1
fi

fail=0
check() {
  local desc=$1
  shift
  if "$@"; then
    echo "  ok  $desc"
  else
    echo "  NG  $desc"
    fail=1
  fi
}

# tcpdump -ttt の 1 行は次の形。awk では空白で区切って $1, $2, ... と数える。
#   $1 経過時間  $2 "IP"  $3 送信元.ポート  $4 ">"  $5 宛先.ポート":"  $6 "Flags"  $7 "[S],"  $8 "seq"  $9 値 ...
# 見る記録は DUMP で決まる。2 本目のクライアントの検査では、呼ぶときに DUMP を差し替える
dump() {
  awk -v client="$CLIENT" -v server="$SERVER.$PORT" "$1" "$DUMP"
}

# --- M4〜5: 3-way handshake ---

syn_recorded() {
  dump 'index($3, client ".") == 1 && $5 == server ":" && $7 == "[S]," { found = 1 }
        END { exit !found }'
}

# SYN|ACK の ACK 番号は、SYN の SEQ + 1 (SYN はシーケンス番号を 1 つ使う。RFC 9293 3.4)。
# SYN と SYN|ACK の行は、tcpdump が相対値に直す前なので本当の値が出る
synack_acks_syn() {
  dump 'index($3, client ".") == 1 && $7 == "[S]," && !have_syn { syn = $9 + 0; have_syn = 1 }
        $3 == server && $7 == "[S.]," && $10 == "ack" && !have_ack { ack = $11 + 0; have_ack = 1 }
        END { exit !(have_syn && have_ack && ack == (syn + 1) % 4294967296) }'
}

# 相手が SYN|ACK を受け入れると、ACK だけのセグメントを返す。
# 自作スタックの SYN|ACK が正しいことを、カーネルの TCP が確かめた結果になる
client_completes_handshake() {
  dump 'index($3, client ".") == 1 && $5 == server ":" && $7 == "[.]," && $8 == "ack" && $9 == "1," { found = 1 }
        END { exit !found }'
}

# どちらかが RST を送っていれば、相手のセグメントを受け入れられなかったことになる
no_rst() {
  dump '$6 == "Flags" && $7 ~ /R/ { found = 1 } END { exit found }'
}

# 相手のデータ (HTTP リクエスト) の末尾までを ACK で受け取ったと伝えている (RFC 9293 3.10.7.4 Seventh)。
# データの行は "seq 1:74," の形で、74 が次に来るはずの番号。それと同じ ACK 番号を返していること
data_acked() {
  dump 'index($3, client ".") == 1 && $5 == server ":" && $8 == "seq" && $9 ~ /:/ && !have_end {
          split($9, r, /[:,]/); end = r[2] + 0; have_end = 1
        }
        have_end && $3 == server && $7 == "[.]," && $8 == "ack" && $9 + 0 == end { found = 1 }
        END { exit !found }'
}

# --- M6: データの送受信 ---

# 自作スタックが送ったデータ (HTTP レスポンス) の末尾までを、クライアントが ACK した。
# サーバのデータの行は "seq 1:110," の形。カーネルの TCP が、自作スタックの SEQ と
# 長さを受け入れたことになる (受け入れなければ、ACK は手前で止まる)
response_acked() {
  dump '$3 == server && $8 == "seq" && $9 ~ /:/ { split($9, r, /[:,]/); end = r[2] + 0; have_end = 1 }
        have_end && index($3, client ".") == 1 && $7 == "[.]," && $8 == "ack" && $9 + 0 == end { found = 1 }
        END { exit !found }'
}

# curl が受け取った本文が、デモの決まった本文と一致する。
# 期待する本文は cmd/tuntcp/main.go の demoBody と同じもの。curl はステータス行とヘッダを
# 解釈し、Content-Length の分だけ本文を受け取ってから書き出すので、HTTP として読めたことも含む
body_received() {
  [[ -f "$OUT/curl.txt" ]] && [[ "$(cat "$OUT/curl.txt")" == "hello from go-tuntcp" ]]
}

# --- M7: 切断 ---

curl_succeeded() {
  [[ -f "$OUT/curl.exit" ]] && [[ "$(cat "$OUT/curl.exit")" == "0" ]]
}

# 自作スタックが FIN を送り、クライアントがその次の番号を ACK した。
# FIN の行は "Flags [F.], seq 87, ack 73," の形で、FIN はシーケンス番号を 1 つ使うので、
# クライアントの ACK は 88 になる。カーネルの TCP が、自作スタックの FIN の番号を受け入れたことになる
our_fin_acked() {
  dump '$3 == server && $7 ~ /F/ && $8 == "seq" && !have_fin { fin = $9 + 0; have_fin = 1 }
        have_fin && index($3, client ".") == 1 && $7 ~ /\./ && $7 !~ /S/ {
          for (i = 8; i < NF; i++) if ($i == "ack" && $(i + 1) + 0 == fin + 1) found = 1
        }
        END { exit !found }'
}

# クライアントの FIN に、その次の番号の ACK を返した (RFC 9293 3.10.7.4 Eighth)
peer_fin_acked() {
  dump 'index($3, client ".") == 1 && $7 ~ /F/ && $8 == "seq" && !have_fin { fin = $9 + 0; have_fin = 1 }
        have_fin && $3 == server && $7 ~ /\./ {
          for (i = 8; i < NF; i++) if ($i == "ack" && $(i + 1) + 0 == fin + 1) found = 1
        }
        END { exit !found }'
}

# クライアントが終わったあとの ss -tn に、自作スタック宛ての接続が残っていない (M7 の完了条件)。
# ss が見せるのはカーネル側のソケットなので、カーネルの TCP が「この接続は終わった」と判断したことになる。
# 自作スタックが FIN を返さなければ FIN-WAIT-2 が、FIN に ACK を返さなければ FIN-WAIT-1 か LAST-ACK が残る。
# ss -tn は TIME-WAIT を出さない (e2e.sh の record_ss を参照)
# ss -tn の 1 行は: $1 State  $2 Recv-Q  $3 Send-Q  $4 Local Address:Port  $5 Peer Address:Port
no_leftover() { # $1: ss の記録
  [[ -s "$1" ]] || return 1
  awk -v server="$SERVER:$PORT" '$5 == server { found = 1 } END { exit found }' "$1"
}

# 2 本目 (つないで、何も送らずに閉じる) の検査は、記録を差し替えて同じ関数を使う
in_close_dump() {
  local DUMP="$OUT/tcpdump-close.txt"
  [[ -s "$DUMP" ]] || return 1
  "$@"
}

echo "--- 検証 ---"
check "クライアントの SYN が届いた" syn_recorded
check "SYN|ACK を返し、ACK 番号が SYN の SEQ + 1 になっている" synack_acks_syn
check "クライアントが SYN|ACK に ACK を返した (handshake の完了)" client_completes_handshake
check "RST が流れていない" no_rst
check "クライアントのデータに、その末尾までの ACK を返した" data_acked
check "クライアントが、レスポンスのデータの末尾までを ACK した" response_acked
check "curl が、決まった HTTP レスポンスの本文を受け取った" body_received
check "curl が終了コード 0 で終わった" curl_succeeded
check "自作スタックが FIN を送り、クライアントがそれを ACK した" our_fin_acked
check "クライアントの FIN に ACK を返した" peer_fin_acked
check "curl のあと、ss -tn に接続が残っていない" no_leftover "$OUT/ss.txt"
echo "--- 検証 (2 本目: クライアントが先に閉じる) ---"
check "クライアントの FIN に ACK を返した" in_close_dump peer_fin_acked
check "自作スタックが FIN を送り、クライアントがそれを ACK した" in_close_dump our_fin_acked
check "RST が流れていない" in_close_dump no_rst
check "クライアントのあと、ss -tn に接続が残っていない" no_leftover "$OUT/ss-close.txt"

exit "$fail"

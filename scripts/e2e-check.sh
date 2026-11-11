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

# curl の最中に取った ss -tn の記録に、自作スタック宛ての接続が ESTAB か、そのあとの状態で出ている。
# ss が見せるのはカーネル (curl 側) のソケットの状態なので、カーネルの TCP が
# 自作スタックの SYN|ACK を受け入れて接続を確立した、という判定になる。
# T6-7 から curl は接続の 1 ミリ秒ほどあとに終わるので、0.1 秒おきの ss では ESTAB を
# 取り逃がし、curl が閉じたあとの FIN-WAIT-1 などが記録に残る。
# FIN-WAIT-1/2、CLOSING、TIME-WAIT は、能動的に開いた側からは ESTABLISHED を経ないと入れない
# (RFC 9293 3.3.2 の図。SYN-RECEIVED からの CLOSE は同時オープンのときだけ) ので、確立の証拠に含める。
# ss -tn の 1 行は: $1 State  $2 Recv-Q  $3 Send-Q  $4 Local Address:Port  $5 Peer Address:Port
ss_estab() {
  [[ -s "$OUT/ss.txt" ]] || return 1
  awk -v client="$CLIENT" -v server="$SERVER:$PORT" \
    '$1 ~ /^(ESTAB|FIN-WAIT-1|FIN-WAIT-2|CLOSING|TIME-WAIT)$/ && index($4, client ":") == 1 && $5 == server { found = 1 }
     END { exit !found }' "$OUT/ss.txt"
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

echo "--- 検証 ---"
check "クライアントの SYN が届いた" syn_recorded
check "SYN|ACK を返し、ACK 番号が SYN の SEQ + 1 になっている" synack_acks_syn
check "クライアントが SYN|ACK に ACK を返した (handshake の完了)" client_completes_handshake
check "RST が流れていない" no_rst
check "クライアントのデータに、その末尾までの ACK を返した" data_acked
check "curl の最中に、ss -tn に ESTAB かそのあとの状態の接続が出ている" ss_estab
check "クライアントが、レスポンスのデータの末尾までを ACK した" response_acked
check "curl が、決まった HTTP レスポンスの本文を受け取った" body_received

exit "$fail"

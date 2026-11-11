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

echo "--- 検証 ---"
check "クライアントの SYN が届いた" syn_recorded
check "SYN|ACK を返し、ACK 番号が SYN の SEQ + 1 になっている" synack_acks_syn
check "クライアントが SYN|ACK に ACK を返した (handshake の完了)" client_completes_handshake
check "RST が流れていない" no_rst

exit "$fail"

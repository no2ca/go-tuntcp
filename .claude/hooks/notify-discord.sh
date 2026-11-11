#!/usr/bin/env bash
# Claude Code の状況を Discord の webhook に短く送る。
#
# 使い方:
#   notify-discord.sh hook          フックから呼ぶ。標準入力のフック入力 (JSON) の種類で文面を決める
#   notify-discord.sh progress TEXT AI が作業の段階を知らせるときに呼ぶ
#
# webhook の URL は秘密なので、リポジトリに入れない .claude/settings.local.json の
# env.DISCORD_WEBHOOK_URL から受け取る。未設定なら何も送らずに終わる。
set -euo pipefail

url="${DISCORD_WEBHOOK_URL:-}"
[ -n "$url" ] || exit 0

project="$(basename "${CLAUDE_PROJECT_DIR:-$PWD}")"

case "${1:-hook}" in
progress)
  shift
  body="📌 [$project] $*"
  ;;
hook)
  case "$(jq -r '.hook_event_name // ""')" in
  Notification) body="🙋 [$project] 確認待ち" ;;
  Stop) body="✅ [$project] 終了" ;;
  *) exit 0 ;;
  esac
  ;;
*)
  echo "unknown mode: $1" >&2
  exit 1
  ;;
esac

jq -n --arg c "$body" '{content: $c, allowed_mentions: {parse: []}}' |
  curl -fsS -m 10 -H 'Content-Type: application/json' -d @- "$url" >/dev/null

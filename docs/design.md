# 設計

いまのコードの構成と、設計の決まりごとを書く。これから変える予定のところは、最後の節にまとめる。

## 範囲

- ひとまずの目標は、自作の型が `net.Listener` と `net.Conn` を満たし、`net/http` のサーバがその上で動くこと。何を含めて何を含めないかは [roadmap.md](roadmap.md) の「ひとまずの目標」に書いてある
- TUN デバイス（L3）から生の IPv4 パケットを読み書きする。Ethernet と ARP は扱わない
- 通信の相手は同じマシンの Linux カーネル。`tun0` に `10.10.0.1 peer 10.10.0.2` を割り当て、カーネルが `10.10.0.1`、自作スタックが `10.10.0.2` になる
- TCP は [RFC 9293](https://datatracker.ietf.org/doc/html/rfc9293) に従う。IPv4 はヘッダのパースと組み立てだけで、フラグメントと IP オプションは扱わない

## パケットの流れ

```
tun0 ──Read──▶ ipv4.Parse ──▶ tcp.Parse ──▶ Stack.Handle ──▶ 返信の IP パケット ──Write──▶ tun0
```

`cmd/tuntcp/main.go` が上の順に呼ぶ。読み取りは別の goroutine（`readLoop`）で行い、チャネルでメインのループに渡す。`Stack` に触るのはメインの goroutine だけなので、ロックは持たない。

## パッケージ

| パッケージ | 役割 |
|---|---|
| `internal/tun` | `/dev/net/tun` を開き、ioctl で TUN デバイスを作る |
| `internal/checksum` | インターネットチェックサム（`Sum` / `Fold`） |
| `internal/ipv4` | IPv4 ヘッダの `Parse` / `Serialize` とチェックサムの検証 |
| `internal/tcp` | TCP ヘッダの `Parse` / `Serialize`、フラグ、シーケンス番号の比較、状態（`State`）、TCB（`ControlBlock`） |
| `internal/stack` | 接続の管理と、受信セグメントの処理 |
| `cmd/tuntcp` | エントリポイント。TUN を開き、80 番を LISTEN し、パケットを表示しながら `Stack` に渡す |

## internal/stack

- `Stack` は LISTEN 中のポート（`listeners`）と、接続のテーブル（`conns`）を持つ
- 接続は 4-tuple（`connKey`: 自分と相手の `netip.AddrPort`）で引く
- `Stack.Handle(ip, tcp, payload)` が受信セグメント 1 つを処理し、返信の IP パケットを返す。返すものがなければ `nil`
  - テーブルに接続があれば `Connection.Handle` に渡す。処理のあと LISTEN か CLOSED に戻った接続は、テーブルから消す
  - 接続がなく、ポートが LISTEN 中なら RFC 9293 3.10.7.2 に従う。SYN なら `Connection` を作って SYN-RECEIVED にし、SYN|ACK を返す
  - どちらでもなければ RFC 9293 3.10.7.1 に従って RST を返す（`buildRST`）
- `Connection` は `tcp.ControlBlock`（状態と、SND / RCV のシーケンス変数）を埋め込む
- `Connection.Handle` は RFC 9293 3.10.7.4 の First〜Eighth の順に並べる。いま中身があるのは Fourth（SYN）と Fifth（ACK）の一部だけ

## 決まりごと

- **プロトコル処理は I/O から切り離す。** `Stack` はパース済みのヘッダを受け取り、バイト列を返すだけにする。TUN、時刻、乱数には直接触らず、外から注入する（いまは `Stack.ISS`）。こうしておくと、テストは実デバイスも root 権限もなしで動く
- **テストは入口から書く。** `Stack.Handle` にセグメントを流し、返信と TCB を見る。内部関数の名前に依存させない
- **シーケンス番号は modulo 2^32 で比べる。** `tcp.SeqLT` / `tcp.SeqLE` を使う
- **RFC の節をコメントに書く。** 差分を RFC と突き合わせて読めるようにする
- **標準ライブラリだけで書く。** 例外は、テストで使う `golang.org/x/net/nettest` だけ

## いまの制限

- `buildSegment` の IPv4 `TotalLen` が 40 固定で、ペイロードを載せられない
- 送信するセグメントに TCP オプションを付けない（`DataOffset` は 5 固定）。受信したオプションは読み捨てる
- `Handle` の戻り値が返信 1 つだけなので、受信をきっかけにしない送信（データの送信、再送）ができない
- 状態は CLOSED、LISTEN、SYN-SENT、SYN-RECEIVED、ESTABLISHED の 5 つだけ

## これから変えるところ

マイルストーンが進むと、いまの「受信 1 つに返信 1 つ」の形では足りなくなる。次の順に広げる。

1. **送信キュー（M6）**: `Stack` が送りたいパケットをキューに積み、`main` がそれを TUN に書く。`Handle` の戻り値に頼らずに送れるようにする
2. **時計の注入（M7）**: タイマー（2MSL、再送、persist）のために、時刻を外から渡す。テストでは時間を手で進める
3. **イベントループ（M8）**: `Stack` を 1 つの goroutine が持ち、受信セグメント、タイマー、API の呼び出しをチャネルで受ける。`net.Listener` と `net.Conn` のメソッドは、このループに要求を送ってブロックする

接続は「入力（セグメント、タイマーの満了、API の呼び出し）を受けて、TCB を更新し、送るセグメントを出す」状態機械として保つ。この形は 3 つの変更を通して変えない。

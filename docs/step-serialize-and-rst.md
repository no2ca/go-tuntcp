# 次のステップ: シリアライズ + TUN への書き戻し（RST 応答）

## なぜこれをやるのか

今のコードはパケットを **読んで表示するだけ** の一方通行になっている。

- [ipv4.go](../ipv4.go) / [tcp.go](../tcp.go) にあるのは `Parse*` だけで、バイト列を組み立てる側（serialize）が無い
- TUN に書き戻すコードも無い

マイルストーン 4（state machine）・5（3-way handshake）は「受信に対して返信する」ことが前提なので、
その前に **ヘッダを組み立てて TUN に書く** 経路を通しておく。

動作確認には **CLOSED 状態の RST 応答** を使う。まだ状態遷移を持っていなくても RFC どおりに正しく
実装できる唯一の応答で、しかも結果が `curl` の挙動でハッキリ分かる（後述）。
これが通れば、SYN-ACK を返すのはフラグとシーケンス番号を差し替えるだけになる。

---

## 1. IPv4 ヘッダのシリアライズ

### 準備: 足りないフィールドを足す

`IPv4Header` には今 `TTL` と `ID` が無い。送信するには TTL が必須（0 だと最初のルータで捨てられる）。
`TTL uint8` と `ID uint16` を struct に追加し、`ParseIPv4Header` でも読むようにする。

> ヘッダ内の位置: ID は `buf[4:6]`、TTL は `buf[8]`。RFC 791 の図と突き合わせてみる。

### `Serialize` を書く

```go
func (h IPv4Header) Serialize(payload []byte) []byte
```

やることは Parse の逆:

1. `20 + len(payload)` バイトのスライスを作る
2. 各フィールドを `binary.BigEndian.PutUint16` などで書き込む
   - IHL は 5 固定でよい（オプション非対応）
   - `TotalLen` は自分で計算する（呼び出し側に任せない）
3. **チェックサム欄を 0 のまま** `calculateIPv4Checksum(buf[:20])` を呼び、結果を `buf[10:12]` に書く
4. 末尾に payload をコピー

### 考えるポイント

- なぜチェックサム欄を 0 にしてから計算するのか？ `Fold` が最後に `^` (ビット反転) しているのと、
  `Verify*` が「計算結果 == 0」で判定していることを結びつけて考えてみる
- `Version` / `IHL` / `TTL` がゼロ値だったら 4 / 5 / 64 を補うようにしておくと、
  呼び出し側が `IPv4Header{Src: ..., Dst: ..., Protocol: PROTO_TCP}` だけ書けば済んで楽

---

## 2. TCP ヘッダのシリアライズ

```go
func (h TCPHeader) Serialize(src, dst [4]byte, payload []byte) []byte
```

`src`, `dst` が要るのは **擬似ヘッダ** のため。TCP チェックサムは IP アドレスも含めて計算する
（すでに `calculateTCPChecksum` が擬似ヘッダを組んでくれるので、そのまま使えばよい）。

### 落とし穴: `DataOffset` の単位

パース時に `hdr.DataOffset = (seg[12] >> 4) * 4` としていて、struct には **バイト数** が入っている
（[tcp.go:50](../tcp.go#L50)）。書き出すときは 4 で割って上位 4 ビットに戻す:

```go
seg[12] = (h.DataOffset / 4) << 4
```

オプションは当面非対応なので、`DataOffset` が 0 なら 20 を補う。

### 手順

1. 20 バイト + payload のスライス
2. 各フィールドを書き込む（Flags は `byte(h.Flags)` でそのまま）
3. チェックサム欄 0 のまま `calculateTCPChecksum(src, dst, seg)` → `seg[16:18]` に書く

---

## 3. RST を組み立てる

新しいファイル `handler.go` あたりに:

```go
func buildRST(ip IPv4Header, tcp TCPHeader, payloadLen int) []byte
```

RFC 9293 **3.10.7.1 (CLOSED STATE)** をそのまま実装する。読んでから書くこと:
https://datatracker.ietf.org/doc/html/rfc9293#section-3.10.7.1

要約すると:

| 受信セグメント | 返すもの |
|---|---|
| RST が立っている | **何も返さない**（RST に RST で返すとループする） |
| ACK が立っている | `Seq = SEG.ACK`, flags = `RST` |
| ACK が無い（SYN など） | `Seq = 0`, `Ack = SEG.SEQ + SEG.LEN`, flags = `RST\|ACK` |

`SEG.LEN` は payload 長だが、**SYN と FIN はそれぞれ 1 を占める**（RFC 9293 3.4）。
curl の最初の SYN に対しては `Ack = SEG.SEQ + 1` になるはず。

IP・ポートの src/dst を入れ替えて、TCP → IPv4 の順で `Serialize` する（内側から包む）。

---

## 4. main のループを双方向にする

### 応答を返せる形に

`displayPacket` は今「表示して終わり」。「表示 + 応答バイト列を返す」に分けるとよい:

```go
func handlePacket(res readResult) []byte   // 返信が無ければ nil
```

`main` はすでに `*os.File` の `tun` を持っているので、`select` の受信ケースで
`tun.Write(reply)` するだけでよい。`readLoop` は `io.Reader` のままで変更不要。

### ついでに直すバグ

[main.go:48-52](../main.go#L48-L52) を見てほしい。`buf` を 1 つ確保して使い回し、
そのままチャネルに渡している。受信側が `res.buf` を読んでいる最中に、次の `tun.Read(buf)` が
同じメモリを上書きしうる。

`append([]byte(nil), buf[:n]...)` でコピーしてから送る。
（なぜ `buf[:n]` をそのまま渡すとダメで、コピーなら安全なのか、スライスと配列の関係で説明できるか？）

---

## 5. テストを書く

`go test ./...` だけで TUN 無しに検証できるようにする。table-driven で。

- `checksum_test.go`: `Sum` の奇数長入力、`Fold` のキャリー畳み込み（`0x1FFFF` → ?）
- `ipv4_test.go`: 既知のバイト列 → Parse → Serialize で **元のバイト列と完全一致**（round-trip）
- `tcp_test.go`: 同上 + `Serialize` の出力を `VerifyTCPChecksum` に通して true
- `handler_test.go`: `buildRST` の 3 ケース（ACK あり / なし / RST 受信で nil）

> round-trip 用のバイト列は、今の `displayPacket` が出す `received N bytes: ...` の hex をそのまま使える。

Taskfile に `test: go test ./...` を足しておくと便利。

---

## 動作確認

1. `go test ./...` が通る
2. `sudo ./go-tuntcp` を起動 → 別ターミナルで `task setup-interface` → `curl 10.10.0.2`
   - **before**: 無応答なので curl がタイムアウトまでハング
   - **after**: `Connection refused` で即失敗 → RST が届いている証拠
3. `sudo tcpdump -ni tun0 -vv` で、送信した RST のチェックサムが `correct` と表示される
   - `incorrect` なら擬似ヘッダか、チェックサム欄の 0 埋め忘れを疑う

---

## 今回やらないこと

- `ControlBlock` を使った状態遷移と SYN-ACK 応答（次のステップ）
- TCP オプション（MSS など）のシリアライズ
- ISS のランダム生成

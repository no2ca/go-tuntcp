# 次のステップ: 状態機械と 3-way handshake（LISTEN → SYN-RECEIVED → ESTABLISHED）

## なぜこれをやるのか

前回までで「受信したセグメントに対して RST を返す」経路が通りました。
これは RFC 9293 でいう **CLOSED 状態の処理** で、「そんな接続は知らない」と答えているだけです。

今回はマイルストーン 4（state machine）と 5（3-way handshake）をまとめて進めます。
この 2 つは別々には作れません。状態機械は「セグメントを受け取って状態を進める」ものであり、
3-way handshake は「LISTEN → SYN-RECEIVED → ESTABLISHED と状態が進むこと」そのものだからです。

ゴールは次の 2 点です。

- `curl 10.10.0.2` が **`Connection refused` にならず** 、`ss -tn` で `ESTAB` と表示される
- TCP の接続が「接続ごとの状態（TCB）」として `map` に保持されている

これができると、以降のマイルストーン（データ送受信・切断・再送）はすべて
「ESTABLISHED の接続に対する処理を足していく」作業になります。

先に読んでおくと理解が早い RFC 9293 の箇所です。

| 節 | 内容 |
|---|---|
| [3.3.2](https://datatracker.ietf.org/doc/html/rfc9293#section-3.3.2) | 状態遷移図（Figure 5）。この図を手元に置いて作業してください |
| [3.4](https://datatracker.ietf.org/doc/html/rfc9293#section-3.4) | シーケンス番号。modulo 2^32 の比較、SYN/FIN が 1 バイト分を占める話 |
| [3.5](https://datatracker.ietf.org/doc/html/rfc9293#section-3.5) | 接続確立。Figure 6 が基本の 3-way handshake |
| [3.10.7.2](https://datatracker.ietf.org/doc/html/rfc9293#section-3.10.7.2) | LISTEN 状態でセグメントを受け取ったときの処理 |
| [3.10.7.4](https://datatracker.ietf.org/doc/html/rfc9293#section-3.10.7.4) | SYN-RECEIVED / ESTABLISHED などの処理 |

---

## 全体像

### 3-way handshake でやり取りされるもの

```
    curl (10.10.0.1:54321)                  自作スタック (10.10.0.2:80)
                                             state = LISTEN
        ---- SYN  seq=C ---------------->    RCV.NXT = C+1, IRS = C
                                             ISS を決める（例: S）
        <--- SYN|ACK seq=S ack=C+1 -----    SND.UNA = S, SND.NXT = S+1
                                             state = SYN-RECEIVED
        ---- ACK  seq=C+1 ack=S+1 ------>    SND.UNA = S+1
                                             state = ESTABLISHED
        ---- PSH|ACK seq=C+1 "GET / ..." ->  (データはマイルストーン 6 以降)
```

**なぜ 2 回ではなく 3 回なのでしょうか。** TCP は双方向なので、双方が「自分の初期シーケンス番号（ISS）」を
相手に伝え、相手からその確認（ACK）をもらう必要があります。SYN で C を伝え、SYN|ACK で「C を受け取った」と
「こちらは S から始める」を同時に伝え、最後の ACK で「S を受け取った」を伝える。片方向の同期に 2 メッセージ必要な
ものを、2 つ目で相乗りさせて 3 回に圧縮しているのが 3-way handshake です（RFC 9293 3.5 の冒頭）。

### コードの構造

```
TUN から読む → ipv4.Parse → tcp.Parse → stack.Handle → 返信があれば TUN に書く
                                              │
                                              ├ 4-tuple で接続テーブルを引く
                                              │    見つかった → その接続の状態に応じた処理 (3.10.7.4)
                                              │    無い + そのポートで LISTEN 中 → LISTEN の処理 (3.10.7.2)
                                              │    無い + LISTEN もしていない → CLOSED の処理 (BuildRST、実装済み)
```

新しく `internal/stack/stack.go` を作り、そこに接続テーブルを持つ `Stack` を置きます。
[handler.go](../internal/stack/handler.go) の `BuildRST` はそのまま CLOSED の処理として使えます。

---

## 1. 接続をどう識別するか: 4-tuple と TCB

### 4-tuple

TCP の接続は **(自分の IP, 自分のポート, 相手の IP, 相手のポート)** の 4 つ組で一意に決まります。
サーバは 80 番を 1 つしか持っていませんが、相手のポート（curl が毎回変える 54321 のような番号）が違えば
別の接続として区別できます。「ポート 80 の接続」ではなく「4-tuple の接続」だと意識してください。

Go では `net/netip` の `AddrPort` が IP とポートの組を表す値型で、**比較可能（comparable）** なので
そのまま map のキーにできます。

```go
// internal/stack/stack.go
type connKey struct {
	Local  netip.AddrPort // 自分側 (10.10.0.2:80)
	Remote netip.AddrPort // 相手側 (10.10.0.1:54321)
}
```

> **なぜ `net.IP` ではなく `netip.Addr` を使っているのでしょうか。** `net.IP` は `[]byte` なので
> `==` で比較できず、map のキーにもできません。`netip.Addr` はこの不便を解消するために
> Go 1.18 で追加された型です。すでに `ipv4.Header` が `netip.Addr` を使っているのはこのためでもあります。
> `netip.AddrPortFrom(ip.Src, t.SrcPort)` で組み立てられます。

### TCB（Transmission Control Block）

RFC は接続ごとの状態をまとめたものを TCB と呼びます。すでに [state.go](../internal/tcp/state.go) に
`tcp.ControlBlock` があり、`State` と送受信のシーケンス変数（`Snd.UNA`, `Snd.NXT`, `Rcv.NXT` など）を持っています。
これに 4-tuple を付けたものを「接続」とします。

```go
type Connection struct {
	Key connKey
	tcp.ControlBlock // 埋め込み。c.State, c.Snd.NXT のように直接触れます
}
```

### Stack

```go
type Stack struct {
	listeners map[uint16]struct{}    // LISTEN 中のポート
	conns     map[connKey]*Connection // 確立中（SYN-RECEIVED 以降）の接続
	ISS       func() uint32           // 初期シーケンス番号の生成 (後述)
}

func New() *Stack
func (s *Stack) Listen(port uint16)
func (s *Stack) Handle(ip ipv4.Header, t tcp.Header, payload []byte) []byte // 返信が無ければ nil
```

**なぜ LISTEN 中のポートは接続テーブルと別に持つのでしょうか。** LISTEN は「相手がまだ決まっていない状態」
なので 4-tuple が作れません。RFC 9293 3.10.7.2 も「LISTEN の TCB は相手のアドレスが未指定」という前提で
書かれています。Linux のソケット API で `listen()` したソケットと `accept()` で返るソケットが別物なのも
同じ理由です。SYN が来て初めて 4-tuple が確定し、そこで `Connection` を作って `conns` に入れます。

### バグになりやすいところ: map の値をポインタにする

`map[connKey]Connection` のように値で持つと、`c := s.conns[key]; c.State = ...` としても
map の中身は変わりません（コピーが書き換わるだけです）。`*Connection` で持ってください。
Go の map の要素はアドレスを取れない（`&s.conns[key]` はコンパイルエラー）ため、
「取り出して書き換えて戻す」か「ポインタで持つ」かの二択になります。

---

## 2. セグメントを組み立てる共通関数

`BuildRST` は RST 専用にヘッダを組み立てていますが、これから SYN|ACK や ACK も送ります。
共通の組み立て関数に切り出してください。

```go
// internal/stack/segment.go あたり
func buildSegment(local, remote netip.AddrPort, flags tcp.Flag, seq, ack uint32, wnd uint16, payload []byte) []byte
```

やることは `BuildRST` の後半と同じです。TCP → IPv4 の順に `Serialize` します。

### バグになりやすいところ

- **`Window` を 0 のまま送らないでください。** `BuildRST` は `Window` を設定していません（RST では問題ありません）が、
  SYN|ACK で Window=0 を広告すると相手は「受信バッファが空いていない」と解釈して **データを一切送ってきません**
  （zero window probe だけが届きます）。当面は `Rcv.WND` を `65535` などの固定値にして、それを毎回入れてください。
- `IPv4 TotalLen` は `BuildRST` で 40 と決め打ちしています。ペイロード付きの ACK を送るようになると
  ずれるので、`20 + len(tcpSegment)` で計算してください（あるいは `ipv4.Header.Serialize` 側で
  `TotalLen` を自動計算するように直しても構いません。前回の手順書ではそちらを勧めていました）。
- `tcp.Header.DataOffset` は今は **32 ビットワード単位** です（[tcp.go:77](../internal/tcp/tcp.go#L77)）。
  オプション無しなら 5 です。

`BuildRST` も内部で `buildSegment` を呼ぶように書き換えるとコードが 1 本にまとまります。
その場合は戻り値が変わるので [handler_test.go](../internal/stack/handler_test.go) の呼び出しも直してください。

---

## 3. シーケンス番号の算術（modulo 2^32）

シーケンス番号は 32 ビットで、いずれ `0xffffffff` から `0` に **巻き戻ります**。
そのため `a < b` のような普通の比較は使えません。`0xfffffff0` と `0x00000010` は、後者のほうが「後」です。

RFC 9293 3.4 は「すべての比較は modulo 2^32 で行う」と述べています。実装の定石は
差を符号付きとして見ることです。

```go
// internal/tcp/seq.go
func SeqLT(a, b uint32) bool { return int32(a-b) < 0 } // a < b
func SeqLE(a, b uint32) bool { return int32(a-b) <= 0 }
```

`a - b` は uint32 の減算なので巻き戻りを含めて正しく「距離」が出ます。それを `int32` に読み替えると、
距離が 2^31 未満なら正、以上なら負になり、「a は b より前か後か」が判定できます
（差が 2^31 ちょうど近辺になることは、ウィンドウが 2^31 より小さい限り起きません）。

> **考えてみてください。** 前回の `buildRST` テストで `Ack wraps around uint32` というケースがありました。
> `t.Seq + segLen` の加算自体は uint32 のオーバーフローで勝手に正しく巻き戻ります。
> 問題になるのは **加算ではなく比較** です。これから書く「SND.UNA < SEG.ACK <= SND.NXT」のような判定で
> `SeqLT` / `SeqLE` を使うようにしてください。

Linux カーネルにも同じ目的の `before()` / `after()` というマクロがあります。テストは
`0xfffffff0` と `0x10` の組で境界をまたぐケースを書いてください。

---

## 4. LISTEN 状態の処理（RFC 9293 3.10.7.2）

`Handle` で接続テーブルに無く、宛先ポートが `listeners` にあるときの処理です。
RFC の記述順そのままに実装してください。

| 順 | 受信セグメント | やること |
|---|---|---|
| 1 | RST が立っている | 無視（`nil`） |
| 2 | ACK が立っている | `<SEQ=SEG.ACK><CTL=RST>` を返す（LISTEN 中の接続に ACK が来るのはおかしい） |
| 3 | SYN が立っている | 下記。接続を作って SYN\|ACK を返す |
| 4 | それ以外 | 無視 |

SYN を受け取ったときにやること（RFC の文章を変数に落としたものです）:

```go
c := &Connection{Key: key}
c.Rcv.IRS = t.Seq
c.Rcv.NXT = t.Seq + 1        // SYN が 1 バイト分を占める
c.Rcv.WND = 65535            // 自分が広告する受信ウィンドウ

c.Snd.ISS = s.ISS()
c.Snd.UNA = c.Snd.ISS
c.Snd.NXT = c.Snd.ISS + 1    // 自分の SYN も 1 バイト分を占める

c.State = tcp.StateSynReceived
s.conns[key] = c

return buildSegment(key.Local, key.Remote, tcp.FlagSYN|tcp.FlagACK, c.Snd.ISS, c.Rcv.NXT, c.Rcv.WND, nil)
```

### バグになりやすいところ

- **`Snd.NXT = ISS + 1` を忘れると次の ACK が受理されません。** SYN-RECEIVED での ACK 判定は
  `SND.UNA < SEG.ACK <= SND.NXT` です。相手は `ack = ISS+1` を返してくるので、`Snd.NXT` が `ISS` のままだと
  「送っていない分を ACK された」扱いになり RST を返してしまいます。
- **`Rcv.NXT = SEG.SEQ + 1` の +1 を忘れると Linux が RST を返してきます。** SYN-SENT 状態の Linux は
  受け取った SYN|ACK の `ack` が `自分の ISS+1` でなければ不正と判断して RST を送ります（RFC 9293 3.10.7.3）。
  `tcpdump` で SYN|ACK の直後に `[R]` が見えたら、まずここを疑ってください。
- 受信した SYN には MSS や Window Scale などの **オプション** が付いています（`tcp.Parse` の 2 番目の戻り値）。
  今回は無視して構いません。オプション無しの SYN|ACK を返すと、相手は「この接続では MSS 536（IPv4 の既定値）、
  ウィンドウスケールもタイムスタンプも無し」として振る舞います。遅くはなりますが正しく動きます。
  なぜ動くのかというと、これらのオプションはすべて「双方が対応しているときだけ使う」設計だからです
  （マイルストーン 10 で扱います）。

### ISS（初期シーケンス番号）

`Stack.ISS` を関数にしているのはテストのためです。本番では `math/rand/v2` の `rand.Uint32()`、
テストでは固定値を返す関数に差し替えます。

**なぜ 0 固定ではいけないのでしょうか。** 同じ 4-tuple の接続が短時間に何度も張り直されたとき、
前の接続の遅延したセグメントが新しい接続の有効なシーケンス番号範囲に入ってしまうと、
まったく別のデータとして受理されてしまいます。ISS を毎回ずらすことでこれを避けます（RFC 9293 3.4.1）。
さらに現代では、第三者が ISS を推測して偽のセグメントを差し込む攻撃を防ぐために、
接続ごとに予測困難な値にすることが推奨されています（RFC 6528）。学習目的では乱数で十分です。

---

## 5. SYN-RECEIVED 状態の処理（RFC 9293 3.10.7.4）

接続テーブルに見つかった接続に対する処理です。RFC 3.10.7.4 は **「状態ごとに手順が書いてある」のではなく
「手順ごとに状態別の分岐が書いてある」** 構造になっています。

```
First:   シーケンス番号の検査
Second:  RST ビットの検査
Third:   セキュリティ (無視してよい)
Fourth:  SYN ビットの検査
Fifth:   ACK フィールドの検査      ← ここで SYN-RECEIVED → ESTABLISHED
Sixth:   URG (無視してよい)
Seventh: セグメントのデータ処理    ← ESTABLISHED なら受信データを処理
Eighth:  FIN ビットの検査          (マイルストーン 7)
```

**この順序には理由があります。** たとえば RST の検査を ACK の検査より前に置くのは、
RST|ACK が来たときに「ACK を処理してから RST で捨てる」という無駄と危険を避けるためです。
また「Fifth で ESTABLISHED になった直後に Seventh でデータを処理する」ことで、
ACK とデータが同じセグメントに乗ってきても 1 回の処理で済みます（RFC は「continue processing」と書いています）。

**コードもこの構造に合わせることを勧めます。** `switch c.State` の中に手順を書くのではなく、
手順を上から順に書き、各手順の中で `switch c.State` する形です。マイルストーンが進んで状態が増えても
RFC と付き合わせて読めるコードになります。

```go
func (s *Stack) handleConn(c *Connection, t tcp.Header, payload []byte) []byte {
	segLen := len(payload)  // SYN / FIN があれば +1 (BuildRST と同じ計算)

	// First: シーケンス番号
	if !c.seqAcceptable(t.Seq, segLen) { ... }

	// Second: RST
	if t.HasFlag(tcp.FlagRST) { ... }

	// Fourth: SYN
	if t.HasFlag(tcp.FlagSYN) { ... }

	// Fifth: ACK
	if !t.HasFlag(tcp.FlagACK) { return nil }
	switch c.State {
	case tcp.StateSynReceived: ...
	case tcp.StateEstablished: ...
	}

	// Seventh: データ (6 節)
	...
}
```

### First: シーケンス番号の検査

「このセグメントは自分の受信ウィンドウに収まっているか」を判定します。RFC の表を関数にしてください。

```go
func (c *Connection) seqAcceptable(seq uint32, segLen int) bool
```

| SEG.LEN | RCV.WND | 受理条件 |
|---|---|---|
| 0 | 0 | `SEG.SEQ == RCV.NXT` |
| 0 | >0 | `RCV.NXT <= SEG.SEQ < RCV.NXT + RCV.WND` |
| >0 | 0 | 受理しない |
| >0 | >0 | `RCV.NXT <= SEG.SEQ < RCV.NXT + RCV.WND` または `RCV.NXT <= SEG.SEQ + SEG.LEN - 1 < RCV.NXT + RCV.WND` |

比較はすべて 3 節の `SeqLT` / `SeqLE` で行ってください。

最後の行が「または」になっている理由を考えてみてください。セグメントの **先頭** がウィンドウ内、
または **末尾** がウィンドウ内、のどちらかなら一部でも新しいデータを含んでいるので捨てない、という意図です。

受理できない場合は、RST でなければ `<SEQ=SND.NXT><ACK=RCV.NXT><CTL=ACK>` を返して終わります。
これは「私が今期待しているのはここです」と相手に教え直す ACK で、後の再送制御の基礎になります。

> handshake の最後の ACK は `seq = IRS+1 = RCV.NXT`、長さ 0 なので、2 行目の条件で受理されます。
> この関数は今後の全マイルストーンで使うので、表の 4 パターンすべてにテストを書いておいてください。

### Second: RST

SYN-RECEIVED で RST を受け取ったら、この接続は passive open（LISTEN から来た）なので
**LISTEN に戻る = 接続をテーブルから消す** だけです（`delete(s.conns, key)`）。返信はしません。

### Fourth: SYN

ウィンドウ内に SYN が来るのは異常です。RFC 9293 は RST を送って接続を捨てるよう書いていますが、
passive open の SYN-RECEIVED については「LISTEN に戻る」だけでよいとも書いています。
今回は接続を消すだけで十分です。

### Fifth: ACK

```go
if !t.HasFlag(tcp.FlagACK) {
	return nil // ACK の無いセグメントは捨てる
}

switch c.State {
case tcp.StateSynReceived:
	if tcp.SeqLT(c.Snd.UNA, t.Ack) && tcp.SeqLE(t.Ack, c.Snd.NXT) {
		c.State = tcp.StateEstablished
		c.Snd.UNA = t.Ack
		c.Snd.WND = t.Window
		c.Snd.WL1 = t.Seq
		c.Snd.WL2 = t.Ack
	} else {
		// 受理できない ACK: <SEQ=SEG.ACK><CTL=RST> を返す
	}
}
```

`SND.UNA < SEG.ACK <= SND.NXT` は「まだ確認されていない **かつ** 実際に送った範囲」を ACK しているか、
という意味です。`SEG.ACK <= SND.UNA` は古い ACK（すでに確認済み）、`SEG.ACK > SND.NXT` は
送ってもいないものへの ACK です。`Snd.WL1` / `Snd.WL2` は「このウィンドウ値をどのセグメントから得たか」を
記録しておくもので、古いセグメントでウィンドウを上書きしないために使います（マイルストーン 9）。

ここで `State` が `ESTABLISHED` に変わったら、そのまま次の手順（Seventh）に進んでください。
`return` してはいけません。

---

## 6. ESTABLISHED での最低限の処理（受信データに ACK を返す）

handshake が終わると curl はすぐに `GET / HTTP/1.1 ...` を送ってきます。
これを何も返さずにいると、curl 側の Linux は「届いていない」と判断して **再送** を繰り返します
（tcpdump で同じ `[P.]` が 0.2 秒、0.4 秒、0.8 秒…と間隔を倍にしながら現れます。これが RTO の指数バックオフです）。

受信バッファはマイルストーン 6 で作るので、今回は **受け取ったデータを捨てつつ ACK だけ返す** ところまでにします。
Seventh の手順として:

```go
if c.State == tcp.StateEstablished && len(payload) > 0 {
	if t.Seq == c.Rcv.NXT {          // 順序どおり届いたものだけ進める
		c.Rcv.NXT += uint32(len(payload))
	}
	return buildSegment(..., tcp.FlagACK, c.Snd.NXT, c.Rcv.NXT, c.Rcv.WND, nil)
}
```

> **これは相手に嘘をついています。** ACK は「ここまで確かに受け取って、あとは私の責任で処理します」という
> 約束です。データを捨てながら ACK するのはその約束を破っていますが、今は手順を確認するための一時的な措置です。
> マイルストーン 6 で受信バッファに入れるようにしたら、この「嘘」は解消されます。

順序が前後した（`t.Seq != c.Rcv.NXT`）ときも ACK は返してください。相手は `ack` の値を見て
「そこから再送すればよい」と分かります。

---

## 7. main の変更

[main.go](../cmd/tuntcp/main.go) では `stack.BuildRST` を直接呼んでいます。これを `Stack` 経由にします。

```go
st := stack.New()
st.Listen(80) // Taskfile の curl が叩くポート

// select の受信ケース
reply := st.Handle(ip, t, tcpPayload)
if reply == nil {
	continue
}
dev.Write(reply)
```

送信したパケットの表示は、`Handle` の戻り値（バイト列）を `ipv4.Parse` → `tcp.Parse` に通せば
今までどおり `displayIPv4` / `displayTCP` で出せます。`Parse` はチェックサムも検証するので、
「自分が組み立てたパケットが自分でパースできる」というセルフチェックも兼ねられます。

### 並行性について

現状は 1 つのゴルーチンが `select` で受信し、`Handle` を呼び、書き戻しているので、
`Stack` の map に **ロックは不要** です。これはいわゆるイベントループの構成で、状態の書き換えが
1 か所からしか起きないことが保証されています。マイルストーン 11 でソケット API を作ると
ユーザのゴルーチンから `Read` / `Write` が呼ばれるようになり、そこで初めて排他が必要になります。
今のうちに「どこが状態を触るか」を意識しておくと、その時に楽になります。

---

## 8. テスト

TUN 無しで `go test ./...` だけで検証できるように書いてください。`Stack.ISS` を固定値にすれば
期待する `Seq` を決め打ちできます。

`internal/tcp/seq_test.go`:

- `SeqLT(0xfffffff0, 0x10) == true`、`SeqLT(0x10, 0xfffffff0) == false`、同値のときの `SeqLT` / `SeqLE`

`internal/stack/stack_test.go`（table-driven で。前回の `handler_test.go` のヘルパが流用できます）:

| ケース | 期待 |
|---|---|
| LISTEN していないポートに SYN | RST\|ACK（前回と同じ。`BuildRST` に委譲されていること） |
| LISTEN 中のポートに SYN | SYN\|ACK、`Seq = ISS`、`Ack = SEG.SEQ+1`、`Window != 0`、接続が `SynReceived` で登録される |
| LISTEN 中のポートに ACK | `<SEQ=SEG.ACK><CTL=RST>`、接続は作られない |
| LISTEN 中のポートに RST | `nil` |
| SYN → 正しい ACK | 2 通目の返信は `nil`、状態が `Established`、`Snd.UNA = ISS+1` |
| SYN → `ack` がずれた ACK | RST、状態は `SynReceived` のまま |
| SYN → RST | 接続がテーブルから消える |
| SYN → ACK → ペイロード付き ACK | 返信は ACK で `Ack = SEG.SEQ + len(payload)` |
| `seqAcceptable` | 表の 4 パターン + ウィンドウ境界 + 巻き戻り |

「SYN → ACK」のように複数セグメントを順に流すテストが増えるので、
`Stack` を 1 つ作って `Handle` を何度も呼ぶ形のヘルパを作ると読みやすくなります。

---

## 動作確認

1. `go test ./...` が通る
2. `sudo ./go-tuntcp` を起動 → 別ターミナルで `sudo task setup-interface` → `curl -m 5 10.10.0.2`
   - **before**: `Connection refused` で即失敗
   - **after**: 5 秒待ってタイムアウト（`Empty reply` など）。**ハングするのが成功です。**
     接続は確立したがサーバが応答を返さないので、curl は待ち続けています
3. 待っている間に別ターミナルで `ss -tn | grep 10.10.0.2`
   - `ESTAB ... 10.10.0.1:XXXXX 10.10.0.2:80` が出れば Linux 側も ESTABLISHED になっています
4. `sudo tcpdump -ni tun0 -vv` で 3 通 + データ + ACK が見える

```
10.10.0.1.54321 > 10.10.0.2.80: Flags [S], seq 123456, win 64240, options [mss 1460,...]
10.10.0.2.80 > 10.10.0.1.54321: Flags [S.], seq 1000, ack 123457, win 65535
10.10.0.1.54321 > 10.10.0.2.80: Flags [.], ack 1, win 64240
10.10.0.1.54321 > 10.10.0.2.80: Flags [P.], seq 1:78, ack 1, win 64240, length 77: HTTP: GET / HTTP/1.1
10.10.0.2.80 > 10.10.0.1.54321: Flags [.], ack 78, win 65535
```

> tcpdump は handshake 以降のシーケンス番号を **相対値** で表示します（`-S` で絶対値になります）。
> `ack 1` は `ISS+1` のことです。

### うまくいかないときの見方

| 症状 | 疑うところ |
|---|---|
| SYN|ACK の直後に curl 側から `[R]` | SYN\|ACK の `ack` が `SEG.SEQ+1` になっていない（4 節） |
| 3 通目の ACK に対して自分が `[R]` を返す | `Snd.NXT = ISS+1` の設定漏れ、または `SeqLT` / `SeqLE` の向き（5 節） |
| `ss` は ESTAB だが `GET` が来ない | SYN\|ACK の `win` が 0（2 節） |
| `GET` が 0.2 秒、0.4 秒… と再送される | 6 節の ACK 返信が無い、または `Ack` の値がずれている |
| チェックサム `incorrect` | `buildSegment` の src/dst の向き（擬似ヘッダは送信側から見た src/dst） |

### 終了後の後始末について

curl を止めると curl 側は FIN を送ってきますが、こちらはまだ FIN を処理しないので
Linux 側の接続は `FIN-WAIT-1` のまま `ss` に残ります。しばらくすると再送を諦めて消えます。

また、自作スタックを再起動すると接続テーブルは空になります。その状態で Linux 側が古い接続の
セグメントを送ってくると、こちらは CLOSED として `BuildRST` で RST を返し、Linux 側もそれを見て
接続を捨てます。**これが RST の本来の役割です。** 「知らない接続については RST で相手に片付けさせる」
仕組みがあるおかげで、片側だけが再起動しても接続の残骸が永遠に残らずに済みます。

---

## 今回やらないこと

- FIN の処理と切断（マイルストーン 7）。curl を止めても FIN-WAIT-1 が残るのはこのためです
- 受信バッファ・送信バッファ、アプリケーションへのデータの受け渡し（マイルストーン 6）
- 再送タイマー。自分が送った SYN|ACK が落ちても再送しません（マイルストーン 8）
- TCP オプション（MSS、Window Scale、Timestamps）の解釈と送信（マイルストーン 10）
- active open（`SYN-SENT`）。自分から接続しにいく側は `Connect` を作るときに（マイルストーン 11）
- 同時オープン、half-open の検出などの稀なケース

---

## 用語の対応表

| RFC の表記 | このコードでの場所 | 意味 |
|---|---|---|
| `SEG.SEQ` / `SEG.ACK` / `SEG.WND` | `t.Seq` / `t.Ack` / `t.Window` | 受信したセグメントの値 |
| `SEG.LEN` | `len(payload)` + SYN/FIN 分 | セグメントが占めるシーケンス番号の数 |
| `SND.UNA` | `c.Snd.UNA` | 送ったが未確認の最古の番号 |
| `SND.NXT` | `c.Snd.NXT` | 次に送る番号 |
| `RCV.NXT` | `c.Rcv.NXT` | 次に受け取ることを期待する番号 |
| `RCV.WND` | `c.Rcv.WND` | 相手に広告する受信ウィンドウ |
| `ISS` / `IRS` | `c.Snd.ISS` / `c.Rcv.IRS` | 自分 / 相手の初期シーケンス番号 |

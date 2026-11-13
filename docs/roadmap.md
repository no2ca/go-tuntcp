# ロードマップ

TUN デバイスの上に TCP プロトコルスタックを実装するまでの、目標、マイルストーン、タスク。
進め方は [CLAUDE.md](../CLAUDE.md)、設計は [design.md](design.md) に書いてある。

- タスクは上から順に片付ける。ID（`T4-3` など）で指定してもよい
- 各マイルストーンの完了条件は、コマンドを実行して確かめられる形にする
- タスクが終わったらチェックを付け、「現在の状態」を直す

## ひとまずの目標

**自作スタックの型が `net.Listener` と `net.Conn` を満たし、標準の `net/http` サーバがその上でそのまま動く。**

`net.Listen` や `net.Dial` 自体に差し込むのではない（あれはカーネルのソケットを使う）。自作の型にインターフェースを満たさせ、`http.Serve(listener, handler)` のように標準ライブラリへ渡せるようにする。対象はサーバ側（passive open）だけ。

達成の条件は次の 4 つ。M12 で確かめる。

1. `http.Serve` に渡した自作の `net.Listener` に対して、`curl` が成功する
2. 大きなデータを上り・下りの両方で送り、ハッシュが一致する
3. パケットを落としても 2 が成り立つ
4. `golang.org/x/net/nettest` の `TestConn` が通る（deadline、並行呼び出し、`Close` の挙動をまとめて確かめるテスト）

### 範囲

| | 含める | 含めない（目標のあと） |
|---|---|---|
| API | `net.Listener`（`Accept` / `Close` / `Addr`）、`net.Conn`（`Read` / `Write` / `Close` / `LocalAddr` / `RemoteAddr` / deadline 3 種） | `Dial`、`CloseWrite`、`SetNoDelay` や `SetKeepAlive` など `*net.TCPConn` 固有のもの |
| TCP | handshake（passive open）、データの送受信、切断、再送、フロー制御、MSS | active open（SYN-SENT）、Window Scale、SACK、輻輳制御、RFC 5961 |

deadline を範囲に入れるのは、`net/http` が使うため。`http.Server` はブロック中の `Read` を中断するのに `SetReadDeadline` を過去の時刻で呼ぶ（Go の `net/http/server.go` の `abortPendingRead`）。何もしない実装では、接続の後始末で止まる。

## 現在の状態（2026-11-13）

- M0〜6（実機確認の自動化、TUN、IPv4、TCP ヘッダ、handshake、データの送受信）は完了。M7 は T7-0 まで済み。次は T7-1
- `curl` が仮のデモ（`cmd/tuntcp` の `demoServer`）から固定の HTTP レスポンスを受け取り、終了コード 0 で終わる
- FIN はまだ扱わない（T7-3）。`curl` が閉じたあと、カーネルは FIN を再送し続け、`ss -tn` には `FIN-WAIT-1` が残る
- `task test` はすべて通る
- `task e2e` は sudo なしで動き、handshake、データへの ACK、`ss -tn` の確立、レスポンスへの ACK、`curl` が受け取った本文を検証して終了コードで返す

## 完了したマイルストーン

- [x] **M0** 実機確認の自動化
  - `task e2e` が `unshare` の名前空間の中で、sudo なしで動く（`scripts/e2e.sh`）
  - 流れたパケットを tcpdump で `tmp/e2e/tcpdump.txt` に記録する
  - `scripts/e2e-check.sh` が記録を検証し、結果を終了コードで返す。記録と判定を分けてあり、記録だけを差し替えて判定を試せる
- [x] **M1** TUN デバイスから IP パケットを読み書きする
- [x] **M2** IPv4 ヘッダのパース、シリアライズ、チェックサム
- [x] **M3** TCP ヘッダのパース、シリアライズ、フラグ、チェックサム
- [x] **M4〜5** 状態機械と 3-way handshake（T4-1〜T4-8）
  - LISTEN → SYN-RECEIVED → ESTABLISHED の passive open。RFC 9293 3.10.7.4 の First（受理判定）、Second（RST）、Fourth（SYN-RECEIVED での SYN）、Fifth（SYN-RECEIVED での ACK の範囲）、Seventh の最小版（データに ACK を返す。保存はしない）
  - RCV.NXT から始まらないデータは、ACK を返してまるごと捨てる（[design.md](design.md) の「対話で決めた仕様」）
  - 段階ごとの分岐とテストの対応は [internal/stack/README.md](../internal/stack/README.md) にある
  - `task e2e` が、handshake の成立、データへの ACK、`curl` の最中の `ss -tn` の `ESTAB` を検証する
  - 後回しにしたもの: RFC 5961 の challenge ACK（目標のあと）、RCV.WND が 0 のときの例外（M10）、URG（扱いを決めていない）

- [x] **M6** データの送受信（T6-1〜T6-7）
  - 受信: RCV.NXT から始まるデータを受信バッファに貯め、`Connection.Read`（仮の口）で読み出す
  - 送信: `Connection.Write`（仮の口）で送信バッファに積み、SND.NXT から相手のウィンドウと MSS 536 に収まる分を切り出して送る。送るパケットは `Stack.TakeOutgoing` の送信キューから出す
  - ESTABLISHED での ACK（Fifth）: SND.UNA を進めて送信バッファから捨て、SND.WL1 / SND.WL2 の条件で送信ウィンドウを更新する
  - `task e2e` が、`curl` の受け取った本文と、レスポンスへの ACK を検証する
  - AI が決めた仕様のうち、仮の読み書き口は [design.md](design.md) の「対話で決めた仕様」にあり、人間の確認待ち。送信キューの形は T7-0 で人間が決め直した
  - 後回しにしたもの: 受信バッファと送信バッファの上限、広告ウィンドウを空きから決めること（M10）、ACK を遅らせてデータに相乗りさせること（決めていない）

---

## M7: 切断

完了条件: `task e2e` で `curl` が終了コード 0 で終わり、そのあと `ss -tn` に接続が残らない

- 終了コード 0 は M6 の時点ですでに満たしている（`curl` は Content-Length の分を受け取ると、FIN の返事を待たずに終わる）。残るのは接続の後始末

- [x] **T7-0** 送る出口を送信キュー 1 つにまとめる
  - `Stack.Handle` の戻り値をなくし、返信も送信キューに積む。振る舞いは変えない。FIN とタイマーのテストを、最初からこの形で書くため
- [x] **T7-1** 状態の追加（FIN-WAIT-1, FIN-WAIT-2, CLOSING, TIME-WAIT, CLOSE-WAIT, LAST-ACK）と stringer の再生成
  - `stringer` を取得できなかったので、`state_string.go` は stringer の出力と同じ形に手で作り直した。名前は `TestState_String` が確かめる
- [x] **T7-2** 時刻の注入とタイマーの基盤
  - テストで時間を進められるよう、時計を差し替え可能にする。M9 の再送でも使う（[design.md](design.md) の「これから変えるところ」の 2）
  - `Stack.Now` で時刻を引き、`Stack.HandleTimeouts` で期限の来たタイマーを処理する。`main` が 1 秒おきに呼ぶ
  - これだけでは外から見える振る舞いがないので、T7-5 とあわせてテストと実装をコミットした
- [x] **T7-3** FIN の受信（Eighth）
  - RCV.NXT を 1 進めて ACK を返す（FIN もシーケンス番号を 1 消費する）
  - 状態ごとの遷移: ESTABLISHED → CLOSE-WAIT、FIN-WAIT-1 → CLOSING、FIN-WAIT-2 → TIME-WAIT
  - FIN-WAIT-1 で受けた FIN が自分の FIN への ACK も運んでいれば、CLOSING ではなく TIME-WAIT に入る（RFC 9293 3.3.2 の Note 2）
  - FIN-WAIT-1 と FIN-WAIT-2 での遷移は、その状態に入る手段（Close）が要るので T7-4 で実装した
- [x] **T7-4** 自分から閉じる（Close）
  - FIN を送る: ESTABLISHED → FIN-WAIT-1、CLOSE-WAIT → LAST-ACK
  - 自分の FIN への ACK を受けたときの遷移: FIN-WAIT-1 → FIN-WAIT-2、CLOSING → TIME-WAIT、LAST-ACK → CLOSED
  - FIN-WAIT-1 と FIN-WAIT-2 で受けるデータと FIN（T7-3 の残り）、各状態での RST もここで実装した
  - AI が決めた仕様（FIN を別のセグメントで送る、FIN にもウィンドウの空きを求める、など）は [design.md](design.md) の「対話で決めた仕様」にあり、人間の確認待ち
- [x] **T7-5** TIME-WAIT の 2MSL タイマー
  - MSL は RFC 9293 3.4.2 の 2 分。TIME-WAIT に入ってから 4 分で接続を消す。相手の FIN の再送を受けたら数え直す
- [ ] **T7-6** 異常時に RST を送って接続を捨てる（Abort）
- [ ] **T7-7** 仮のデモでレスポンスのあとに閉じ、`task e2e` の検証に `curl` の終了コードと接続の後始末を足す
  - 終了コードは `tmp/e2e/curl.exit` に記録済み（T6-7）。検証にはまだ入れていない

## M8: Socket API（サーバ側）

再送とフロー制御より先に API を入れる。送受信バッファを、並行処理の形が決まってから仕上げるため。

完了条件: `task e2e` で、`http.Serve` に渡した自作の `net.Listener` に `curl` が成功する。`go test -race ./...` が通る

- [ ] **T8-1** API とパッケージの形を決める
  - 公開するパッケージの名前と置き場所、型の名前、`Stack` の起動と停止の方法。案と理由を示して、人間が決める
- [ ] **T8-2** イベントループ
  - `Stack` を 1 つの goroutine が持ち、受信セグメント、タイマー、API の呼び出しをチャネルで受ける（[design.md](design.md) の「これから変えるところ」の 3）
- [ ] **T8-3** `net.Listener`: `Accept` / `Close` / `Addr`
  - ESTABLISHED になった接続を accept キューに積む。キューの上限と、溢れたときの扱いを決める
  - `Close` したら、ブロック中の `Accept` は `net.ErrClosed` で返る
- [ ] **T8-4** `net.Conn`: `Read` / `Write` / `Close` / `LocalAddr` / `RemoteAddr`
  - `Read` はデータが来るまでブロックする。`Write` は送信バッファに空きができるまでブロックする
  - `Close` は送信バッファの残りを送ってから FIN を送る。呼び出し自体は待たずに返る
- [ ] **T8-5** エラーの約束
  - 相手の FIN を受け、受信バッファを読み切ったら `Read` は `io.EOF`
  - 相手の RST を受けたら、`Read` と `Write` は `syscall.ECONNRESET` を包んだエラー
  - `Close` のあとの呼び出しと、`Close` で中断された呼び出しは `net.ErrClosed`
  - エラーは標準の `net` と同じく `*net.OpError` で包む
- [ ] **T8-6** deadline: `SetDeadline` / `SetReadDeadline` / `SetWriteDeadline`
  - 期限が来たら、ブロック中の呼び出しも `os.ErrDeadlineExceeded` で返る。エラーは `net.Error` を満たし、`Timeout()` が true を返す
  - 過去の時刻を渡すと、ブロック中の呼び出しがすぐ返る。ゼロ値を渡すと期限がなくなる
- [ ] **T8-7** 並行呼び出し
  - 複数の goroutine から `Read`、`Write`、`Close`、deadline の設定を同時に呼べる。`-race` を付けたテストで確かめる
- [ ] **T8-8** `net/http` のサーバを載せる
  - 仮のデモを捨て、`cmd/` の下のサンプルを `http.Serve` に置き換える
  - `task e2e` の検証を、`http.Serve` が返すステータスと本文に合わせる

## M9: 再送

完了条件: パケットを落としても、`curl` での転送が最後まで終わる

- [ ] **T9-1** 再送キュー（送信済みで ACK 待ちのセグメントと、その送信時刻）
- [ ] **T9-2** ACK で再送キューから取り除き、RTO が切れたら先頭を再送する
  - SYN|ACK と FIN も再送の対象になる
- [ ] **T9-3** RTT の計測と RTO の計算（[RFC 6298](https://datatracker.ietf.org/doc/html/rfc6298)）
  - SRTT と RTTVAR の更新式。再送したセグメントでは RTT を測らない（Karn のアルゴリズム）
- [ ] **T9-4** RTO の指数バックオフと、再送回数の上限
  - 上限に達したら接続を捨て、`Read` と `Write` にタイムアウトのエラーを返す
- [ ] **T9-5** 順序が入れ替わったセグメントの扱い
  - 先に届いた後ろのセグメントを捨てるか、取っておくかを決める。捨てる場合も重複 ACK を返す
- [ ] **T9-6** `task e2e` でパケットを落とす
  - 名前空間の中で `tc netem` を `tun0` に付ける。`sch_netem` はこの環境のカーネルにモジュールとして入っているが、名前空間の中から使えるかはまだ確かめていない
  - 使えなければ、自作スタックの TUN への読み書きの手前で、決まった割合を落とす仕組みを入れる

## M10: フロー制御

完了条件: アプリが `Read` を止めると相手の送信が止まり、再開すると転送が続く。相手が受け取らないとき、`Write` がブロックする

- [ ] **T10-1** 受信バッファに上限を設け、空きから RCV.WND を決めて広告する
- [ ] **T10-2** ゼロウィンドウ probe（persist タイマー）
  - 相手のウィンドウが 0 の間、probe を送り続ける（RFC 9293 [3.8.6.1](https://datatracker.ietf.org/doc/html/rfc9293#section-3.8.6.1)）
- [ ] **T10-3** SWS 回避（受信側）と Nagle（送信側）（RFC 9293 [3.8.6.2](https://datatracker.ietf.org/doc/html/rfc9293#section-3.8.6.2)）
- [ ] **T10-4** `task e2e` で、読み出しの遅いハンドラと、受け取りの遅いクライアント（`curl --limit-rate`）を試す

## M11: MSS

完了条件: SYN|ACK に MSS が載り、`ss -ti` で相手側がそれを認識している。送るセグメントが相手の MSS を超えない

- [ ] **T11-1** オプションのパースとシリアライズ（kind / length / value）
- [ ] **T11-2** MSS の交換
  - SYN で受け取った MSS から送信サイズを決め、SYN|ACK で自分の MSS を送る。536 固定をやめる

## M12: 目標の達成を確かめる

完了条件: 「ひとまずの目標」の 4 つの条件がすべて `task` のコマンドで確かめられる

- [ ] **T12-1** 大きなデータの上りと下り
  - `http.Serve` のハンドラで、大きな本文を返す経路と、受け取った本文のハッシュを返す経路を作る。`curl` で両方向を試し、ハッシュを比べる
  - パケットを落とした状態でも同じことを確かめる
- [ ] **T12-2** `nettest.TestConn` を通す
  - `golang.org/x/net/nettest` をテストに限って使う。標準ライブラリだけという決まりの、唯一の例外
  - `TestConn` は接続の両端を `net.Conn` として受け取る。自作スタックは自分から接続できないので、片方は自作の `Accept` が返した接続、もう片方はカーネルの `net.Dial` が返した接続にする
  - TUN が要るので、`task test` ではなく名前空間の中で動かす。ビルドタグで分ける
- [ ] **T12-3** tcpdump の記録から、handshake・再送・切断が RFC どおりに進むことを `task e2e` で検証する
- [ ] **T12-4** README と design.md を、できあがった API の使い方に合わせて書き直す

## 目標のあと（まだ分割していない）

- `Dial`: active open と SYN-SENT の処理（RFC 9293 [3.10.7.3](https://datatracker.ietf.org/doc/html/rfc9293#section-3.10.7.3)）、エフェメラルポートの割り当て。`http.Client` を自作スタック経由で使えるようになる
  - 両端を自作スタックにできるので、2 つの `Stack` をメモリの中でつなぎ、TUN なしで `nettest.TestConn` を動かせる
- `CloseWrite`（half-close）
- Window Scale（[RFC 7323](https://datatracker.ietf.org/doc/html/rfc7323)）
- RFC 5961（RST と SYN への challenge ACK）
- 輻輳制御（slow start, congestion avoidance, fast retransmit）
- SACK
- IPv6
- 性能の測定と最適化

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

## 現在の状態（2026-11-11）

- M0〜3（実機確認の自動化、TUN、IPv4、TCP ヘッダ）は完了
- M4〜5 は途中。LISTEN → SYN-RECEIVED → ESTABLISHED の基本の経路は動き、`curl 10.10.0.2` の SYN に SYN|ACK を返せる
- `internal/stack` のテストは 21 件中 10 件が失敗する。T4-3、T4-4、T4-6、T4-7 のテストを先に書いてあるため
- `task e2e` は sudo なしで動き、tcpdump の記録から 3-way handshake の成立を検証して終了コードで返す
- `curl` 自体はまだレスポンスを受け取れず、5 秒でタイムアウトする。`task e2e` はこれを失敗として扱っていない（T6-7 で検証に足す）

## 完了したマイルストーン

- [x] **M0** 実機確認の自動化
  - `task e2e` が `unshare` の名前空間の中で、sudo なしで動く（`scripts/e2e.sh`）
  - 流れたパケットを tcpdump で `tmp/e2e/tcpdump.txt` に記録する
  - `scripts/e2e-check.sh` が記録を検証し、結果を終了コードで返す。記録と判定を分けてあり、記録だけを差し替えて判定を試せる
- [x] **M1** TUN デバイスから IP パケットを読み書きする
- [x] **M2** IPv4 ヘッダのパース、シリアライズ、チェックサム
- [x] **M3** TCP ヘッダのパース、シリアライズ、フラグ、チェックサム

---

## M4〜5: 状態機械と 3-way handshake（残り）

完了条件: `task test` がすべて通る。`curl 10.10.0.2` の最中に `ss -tn` で `ESTAB` が見える

- [x] **T4-1** `StringFlags` の表示順とテストを揃える
- [x] **T4-2** 接続を接続テーブルから消す仕組み
  - 接続が LISTEN か CLOSED に戻ったら、`Stack.Handle` が `conns` から削除する
- [ ] **T4-3** セグメントの受理判定（First: シーケンス番号の検査）
  - RFC 9293 [3.10.7.4](https://datatracker.ietf.org/doc/html/rfc9293#section-3.10.7.4) First の表（SEG.LEN と RCV.WND がそれぞれ 0 かどうかで 4 通り）
  - 受理できなければ `<SEQ=SND.NXT><ACK=RCV.NXT><CTL=ACK>` を返して捨てる。RST が立っていれば何も返さない
  - modulo 2^32 の比較には `tcp.SeqLT` / `tcp.SeqLE` を使う
  - テスト: `SynReceived_OldSeq_ReACKed`, `SynReceived_RetransmittedSYN`, `Established_UnexpectedSeq`, `Established_ZeroWindow`
- [ ] **T4-4** RST の受信（Second）
  - SYN-RECEIVED（passive open）なら LISTEN に戻る。ESTABLISHED なら接続を閉じる
  - テスト: `SynReceived_RST_ReturnsToListen`, `Established_RST_Closes`
  - RFC 5961 の challenge ACK は後回し（目標のあと）
- [ ] **T4-5** ウィンドウ内の SYN（Fourth）を受けたら LISTEN に戻す
  - 実装は済んでいて、テストも通る。T4-3 と T4-4 を入れたあとも通ることを確かめてからチェックを付ける
  - テスト: `SynReceived_NewSYNInWindow_ReturnsToListen`
- [ ] **T4-6** SYN-RECEIVED での ACK の範囲チェック（Fifth）
  - `SND.UNA < SEG.ACK =< SND.NXT` を満たさなければ `<SEQ=SEG.ACK><CTL=RST>` を返す
  - テスト: `SynReceived_BadACK`, `ISSWrapsAround`
- [ ] **T4-7** 順序どおりのデータを受け取って ACK を返す（Seventh の最小版）
  - RCV.NXT を SEG.LEN だけ進め、`<SEQ=SND.NXT><ACK=RCV.NXT><CTL=ACK>` を返す。データはまだ保存しない（T6-2）
  - テスト: `Established_Data`, `ClientSeqWrapsAround`, `MultipleConnections`
- [ ] **T4-8** `task e2e` で ESTAB を確かめる
  - `curl` の最中の `ss -tn` を `tmp/e2e/` に記録し、`ESTAB` が出ることを `scripts/e2e-check.sh` の検証に足す

## M6: データの送受信

完了条件: `task e2e` で、`curl` が固定の HTTP レスポンスを受け取る

- [ ] **T6-1** ペイロード付きセグメントの組み立て
  - `buildSegment` の IPv4 `TotalLen` が 40 固定なので、ペイロード長から計算する
- [ ] **T6-2** 受信バッファ
  - 受理したデータを貯め、アプリ側が読み出せる型。T4-7 から呼ぶ
  - この時点では大きさを制限せず、広告ウィンドウも固定のままにする（M10 で直す）
- [ ] **T6-3** 送信の配管
  - いまは `Handle` の戻り値でしか送れない。受信を待たずに送れるよう、`Stack` から TUN への送信キューを作る（[design.md](design.md) の「これから変えるところ」の 1）
- [ ] **T6-4** 送信バッファと、`Connection` への仮の書き込み口
- [ ] **T6-5** 送信バッファからセグメントを切り出して送る
  - SND.NXT から、相手のウィンドウ（`SND.UNA + SND.WND` まで）と MSS（当面 536 固定）に収まる分だけ送り、SND.NXT を進める
- [ ] **T6-6** ESTABLISHED での ACK の処理（Fifth）
  - `SND.UNA < SEG.ACK =< SND.NXT` なら SND.UNA を進める。重複した ACK は無視し、まだ送っていない範囲への ACK には ACK を返して捨てる
  - 送信ウィンドウを更新する（SND.WL1 と SND.WL2 を使う条件）
- [ ] **T6-7** 固定の HTTP レスポンスを返す仮のデモと、`task e2e` の検証
  - リクエストを受け取ったら決まったレスポンスを送る処理を `main` に直接書く。M8 で API ができたら捨てる
  - `curl` がレスポンスを受け取ることを `scripts/e2e-check.sh` に足す

## M7: 切断

完了条件: `task e2e` で `curl` が終了コード 0 で終わり、そのあと `ss -tn` に接続が残らない

- [ ] **T7-1** 状態の追加（FIN-WAIT-1, FIN-WAIT-2, CLOSING, TIME-WAIT, CLOSE-WAIT, LAST-ACK）と stringer の再生成
- [ ] **T7-2** 時刻の注入とタイマーの基盤
  - テストで時間を進められるよう、時計を差し替え可能にする。M9 の再送でも使う（[design.md](design.md) の「これから変えるところ」の 2）
- [ ] **T7-3** FIN の受信（Eighth）
  - RCV.NXT を 1 進めて ACK を返す（FIN もシーケンス番号を 1 消費する）
  - 状態ごとの遷移: ESTABLISHED → CLOSE-WAIT、FIN-WAIT-1 → CLOSING、FIN-WAIT-2 → TIME-WAIT
- [ ] **T7-4** 自分から閉じる（Close）
  - FIN を送る: ESTABLISHED → FIN-WAIT-1、CLOSE-WAIT → LAST-ACK
  - 自分の FIN への ACK を受けたときの遷移: FIN-WAIT-1 → FIN-WAIT-2、CLOSING → TIME-WAIT、LAST-ACK → CLOSED
- [ ] **T7-5** TIME-WAIT の 2MSL タイマー
- [ ] **T7-6** 異常時に RST を送って接続を捨てる（Abort）
- [ ] **T7-7** 仮のデモでレスポンスのあとに閉じ、`task e2e` の検証に `curl` の終了コードと接続の後始末を足す

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

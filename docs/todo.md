# TODO

[milestones.md](milestones.md) のマイルストーンを小さな TODO に分けたもの。イシューのように 1 件ずつ片付ける。

## 進め方

- 各 TODO に担当を付ける
  - **[人間]**: プロトコルとして大事な判断ロジック。RFC を読んで自分で書く
  - **[LLM]**: 配管、データ構造、表示、テスト、リファクタ、実機確認の手順
- コード量の比率は LLM : 人間 = 8:2〜9:1 を目安にする
  - 人間のタスクは 1 件あたり数十行に収まるよう、周辺は LLM が先に用意する
  - 件数ではなく行数の比率なので、人間のタスクの件数が多めに見えても構わない

### 担当の決め方

人間が書くもの:

- 受信したセグメントをどう判定し、状態とシーケンス変数をどう更新するか（RFC 9293 3.10 のイベント処理の本体）
- 再送のタイミングやウィンドウの大きさなど、TCP の振る舞いを決める計算

LLM が書くもの:

- 人間のタスクに必要な型、関数シグネチャ、呼び出し元の配管
- テスト、表示、ログ、Taskfile、ドキュメント
- 人間が一度書いた処理と同じ形の繰り返し（状態が増えたときの分岐の追加など）

### 1 件の流れ

1. 「T4-3 をやって」のように ID で依頼する
2. [LLM] のタスク: LLM が実装し、差分とテスト結果を報告する。人間は差分を読み、納得したらコミットする
3. [人間] のタスク: LLM が先にテストとスタブ（中身が `// TODO(human)` の関数）を用意し、要点を短く説明する。人間が中身を書いてテストを通す。詰まったら LLM にヒントを求める
4. 終わったらチェックを付ける

[docs/step-*.md](.) は以前の方針（全部を人間が書く）で作った詳しい手順書。参考として残す。

## 現在の状態（2026-09-23）

- M1〜3 は完了
- M4〜5 は途中。LISTEN → SYN-RECEIVED → ESTABLISHED の基本経路は動く
- `internal/stack` のテストは 21 件中 10 件が失敗。未実装の部分のテストを先に書いてあるため

---

## M4〜5: 状態機械と 3-way handshake（残り）

完了条件: `go test ./...` がすべて通る。`curl 10.10.0.2` の最中に `ss -tn` で `ESTAB` が見える

- [x] **T4-1** [LLM] `StringFlags` の表示順とテストを揃える
  - 作業ツリーで表示順を FIN→URG に変えたが、テストは URG→FIN を期待している。どちらに揃えるか決めてから直す
- [x] **T4-2** [LLM] 接続を接続テーブルから消す仕組み
  - 接続が LISTEN か CLOSED に戻ったら、`Stack.Handle` が `conns` から削除する
  - T4-4 と T4-5 の前提
- [ ] **T4-3** [人間] セグメントの受理判定（First: シーケンス番号の検査）
  - RFC 9293 [3.10.7.4](https://datatracker.ietf.org/doc/html/rfc9293#section-3.10.7.4) First の表（SEG.LEN と RCV.WND がそれぞれ 0 かどうかで 4 通り）
  - 受理できなければ `<SEQ=SND.NXT><ACK=RCV.NXT><CTL=ACK>` を返して捨てる。RST が立っていれば何も返さない
  - modulo 2^32 の比較には `tcp.SeqLT` / `tcp.SeqLE` を使う
  - テスト: `SynReceived_OldSeq_ReACKed`, `SynReceived_RetransmittedSYN`, `Established_UnexpectedSeq`, `Established_ZeroWindow`
- [ ] **T4-4** [人間] RST の受信（Second）
  - SYN-RECEIVED（passive open）なら LISTEN に戻る。ESTABLISHED なら接続を閉じる
  - テスト: `SynReceived_RST_ReturnsToListen`, `Established_RST_Closes`
  - RFC 5961 の challenge ACK は後回し（M13）
- [ ] **T4-5** [LLM] ウィンドウ内の SYN（Fourth）を受けたら LISTEN に戻す
  - T4-4 と同じ形
  - テスト: `SynReceived_NewSYNInWindow_ReturnsToListen`
- [ ] **T4-6** [人間] SYN-RECEIVED での ACK の範囲チェック（Fifth）
  - `SND.UNA < SEG.ACK =< SND.NXT` を満たさなければ `<SEQ=SEG.ACK><CTL=RST>` を返す
  - テスト: `SynReceived_BadACK`, `ISSWrapsAround`
- [ ] **T4-7** [人間] 順序どおりのデータを受け取って ACK を返す（Seventh の最小版）
  - RCV.NXT を SEG.LEN だけ進め、`<SEQ=SND.NXT><ACK=RCV.NXT><CTL=ACK>` を返す。データはまだ保存しない（T6-2）
  - テスト: `Established_Data`, `ClientSeqWrapsAround`, `MultipleConnections`
- [ ] **T4-8** [LLM] 実機で ESTAB を確かめる手順を Taskfile に追加する
  - `ss -tn` と `tcpdump -i tun0` のタスク

## M6: データの送受信

完了条件: curl の HTTP リクエストを受け取り、固定の HTTP レスポンスを返せる

- [ ] **T6-1** [LLM] ペイロード付きセグメントの組み立て
  - `buildSegment` の IPv4 `TotalLen` が 40 固定なので、ペイロード長から計算する
- [ ] **T6-2** [LLM] 受信バッファ
  - 受理したデータを貯め、アプリ側が読み出せる型。T4-7 から呼ぶ
- [ ] **T6-3** [LLM] 送信の配管
  - いまは `Handle` の戻り値でしか送れない。受信を待たずに送れるよう、stack から TUN への送信キューを作る
- [ ] **T6-4** [LLM] 送信バッファと、`Connection` への仮の書き込み口
- [ ] **T6-5** [人間] 送信バッファからセグメントを切り出して送る
  - SND.NXT から、相手のウィンドウ（`SND.UNA + SND.WND` まで）と MSS（当面 536 固定）に収まる分だけ送り、SND.NXT を進める
- [ ] **T6-6** [人間] ESTABLISHED での ACK の処理（Fifth）
  - `SND.UNA < SEG.ACK =< SND.NXT` なら SND.UNA を進める。重複した ACK は無視し、まだ送っていない範囲への ACK には ACK を返して捨てる
  - 送信ウィンドウを更新する（SND.WL1 と SND.WL2 を使う条件）
- [ ] **T6-7** [LLM] 固定の HTTP レスポンスを返すデモ（main から使う）

## M7: 切断

完了条件: curl が正常に終了し、`ss -tn` に接続が残らない

- [ ] **T7-1** [LLM] 状態の追加（FIN-WAIT-1, FIN-WAIT-2, CLOSING, TIME-WAIT, CLOSE-WAIT, LAST-ACK）と stringer の再生成
- [ ] **T7-2** [LLM] 時刻の注入とタイマーの基盤
  - テストで時間を進められるよう、時計を差し替え可能にする。M8 の再送でも使う
- [ ] **T7-3** [人間] FIN の受信（Eighth）
  - RCV.NXT を 1 進めて ACK を返す（FIN もシーケンス番号を 1 消費する）
  - 状態ごとの遷移: ESTABLISHED → CLOSE-WAIT、FIN-WAIT-1 → CLOSING、FIN-WAIT-2 → TIME-WAIT
- [ ] **T7-4** [人間] 自分から閉じる（Close）
  - FIN を送る: ESTABLISHED → FIN-WAIT-1、CLOSE-WAIT → LAST-ACK
  - 自分の FIN への ACK を受けたときの遷移: FIN-WAIT-1 → FIN-WAIT-2、CLOSING → TIME-WAIT、LAST-ACK → CLOSED
- [ ] **T7-5** [LLM] TIME-WAIT の 2MSL タイマー
- [ ] **T7-6** [LLM] 異常時に RST を送って接続を捨てる（Abort）

## M8: 再送

完了条件: `tc netem` でパケットを落としても転送が最後まで終わる

- [ ] **T8-1** [LLM] 再送キュー（送信済みで ACK 待ちのセグメントと、その送信時刻）
- [ ] **T8-2** [人間] ACK で再送キューから取り除き、RTO が切れたら先頭を再送する
  - SYN|ACK と FIN も再送の対象になる
- [ ] **T8-3** [人間] RTT の計測と RTO の計算（[RFC 6298](https://datatracker.ietf.org/doc/html/rfc6298)）
  - SRTT と RTTVAR の更新式。再送したセグメントでは RTT を測らない（Karn のアルゴリズム）
- [ ] **T8-4** [LLM] RTO の指数バックオフと、再送回数の上限
- [ ] **T8-5** [LLM] netem でパケットロスを起こす Taskfile のタスク

## M9: フロー制御

- [ ] **T9-1** [人間] 受信バッファの空きから RCV.WND を決めて広告する
- [ ] **T9-2** [人間] ゼロウィンドウ probe（persist タイマー）
  - 相手のウィンドウが 0 の間、probe を送り続ける（RFC 9293 [3.8.6.1](https://datatracker.ietf.org/doc/html/rfc9293#section-3.8.6.1)）
- [ ] **T9-3** [LLM] SWS 回避（受信側）と Nagle（送信側）（RFC 9293 [3.8.6.2](https://datatracker.ietf.org/doc/html/rfc9293#section-3.8.6.2)）

## M10: TCP オプション

- [ ] **T10-1** [LLM] オプションのパースとシリアライズ（kind / length / value）
- [ ] **T10-2** [人間] MSS の交換
  - SYN で受け取った MSS から送信サイズを決め、SYN|ACK で自分の MSS を送る
- [ ] **T10-3** [人間] Window Scale（[RFC 7323](https://datatracker.ietf.org/doc/html/rfc7323)）
  - SYN のときだけ交換し、それ以降は SEG.WND をシフトして解釈する
- SACK は M13 に回す

## M11: Socket API

- [ ] **T11-1** [LLM] 並行処理の設計: stack を 1 つの goroutine のイベントループにし、API からはチャネルで要求を送る
- [ ] **T11-2** [LLM] `Listen` / `Accept`（accept キュー）
- [ ] **T11-3** [LLM] `Read` / `Write` / `Close` のブロッキング実装（`net.Conn` に寄せる）
- [ ] **T11-4** [人間] `Connect`: SYN-SENT の処理（RFC 9293 [3.10.7.3](https://datatracker.ietf.org/doc/html/rfc9293#section-3.10.7.3)）
  - SYN|ACK を受けたら ESTABLISHED。SYN だけ受けたら（同時オープン）SYN-RECEIVED

## M12: 実環境で通信

- [ ] **T12-1** [LLM] Socket API を使った HTTP サーバのサンプル（`cmd/` の下）
- [ ] **T12-2** [LLM] 大きなデータの送受信の確認（`nc` やファイル転送）
- [ ] **T12-3** [人間] tcpdump か Wireshark で自作スタックの通信を観察し、handshake・再送・切断を確かめる（コードは書かない）

## M13: 発展（まだ分割していない）

- RFC 5961（RST と SYN への challenge ACK）
- 輻輳制御（slow start, congestion avoidance, fast retransmit）
- SACK
- IPv6
- 性能の測定と最適化

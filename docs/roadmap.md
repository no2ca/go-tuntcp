# ロードマップ

TUN デバイスの上に TCP プロトコルスタックを実装するまでの、マイルストーンとタスク。
進め方は [CLAUDE.md](../CLAUDE.md)、設計は [design.md](design.md) に書いてある。

- タスクは上から順に片付ける。ID（`T4-3` など）で指定してもよい
- 各マイルストーンの完了条件は、コマンドを実行して確かめられる形にする
- タスクが終わったらチェックを付け、「現在の状態」を直す

## 現在の状態（2026-11-11）

- M1〜3（TUN、IPv4、TCP ヘッダ）は完了
- M4〜5 は途中。LISTEN → SYN-RECEIVED → ESTABLISHED の基本の経路は動き、`curl 10.10.0.2` の SYN に SYN|ACK を返せる
- `internal/stack` のテストは 21 件中 10 件が失敗する。T4-3、T4-4、T4-6、T4-7 のテストを先に書いてあるため
- M0 は完了。実機確認（`task e2e`）は sudo なしで動き、tcpdump の記録から 3-way handshake の成立を検証して終了コードで返す
- `curl` 自体はまだレスポンスを受け取れず、5 秒でタイムアウトする。`task e2e` はこれを失敗として扱っていない（M6 で検証に足す）

## 完了したマイルストーン

- [x] **M1** TUN デバイスから IP パケットを読み書きする
- [x] **M2** IPv4 ヘッダのパース、シリアライズ、チェックサム
- [x] **M3** TCP ヘッダのパース、シリアライズ、フラグ、チェックサム

---

## M0: 実機確認の自動化（完了）

AI が実機確認まで自分で回せるようにする。

完了条件: `task e2e` が sudo なしで動き、期待どおりでなければ 0 以外の終了コードを返す

- [x] **T0-1** `task e2e` を sudo なしで動かす
  - `unshare -Urn` で作ったユーザー名前空間とネットワーク名前空間の中で、バイナリの起動、IP の割り当て、`curl` を行う
  - `tun0` は名前空間の中にだけでき、スクリプトが終わると消える。`setcap` は要らなくなった
  - 別のターミナルから手で試すためのタスク（`setup-interface` など）は、ホスト側の `tun0` を使うので `sudo` のまま残す
- [x] **T0-2** `task e2e` で流れたパケットを tcpdump で記録する
  - 名前空間の中で `tcpdump -i tun0 -n -ttt tcp` を回し、`tmp/e2e/tcpdump.txt` に残して最後に表示する。1 セグメントが 1 行になる
  - 自作スタックの出力（16 進ダンプなど）は `tmp/e2e/stack.log` に残す
  - 状態遷移のような、tcpdump に見えない内部のログは、必要になったときに足す
- [x] **T0-3** `task e2e` に検証を入れる
  - `scripts/e2e-check.sh` が `tmp/e2e/` の記録を読み、期待する結果を確かめて終了コードで返す。いまは 3-way handshake の 4 項目を見る
  - 記録を取る `e2e.sh` と、判定する `e2e-check.sh` を分けてある。記録だけを差し替えて判定を試せる
  - マイルストーンごとの確認（ESTAB が見える、HTTP レスポンスが返る、接続が残らない）は、それぞれのタスクでここに足していく

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
  - RFC 5961 の challenge ACK は後回し（M13）
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

完了条件: curl の HTTP リクエストを受け取り、固定の HTTP レスポンスを返せる

- [ ] **T6-1** ペイロード付きセグメントの組み立て
  - `buildSegment` の IPv4 `TotalLen` が 40 固定なので、ペイロード長から計算する
- [ ] **T6-2** 受信バッファ
  - 受理したデータを貯め、アプリ側が読み出せる型。T4-7 から呼ぶ
- [ ] **T6-3** 送信の配管
  - いまは `Handle` の戻り値でしか送れない。受信を待たずに送れるよう、`Stack` から TUN への送信キューを作る（[design.md](design.md) の「これから変えるところ」の 1）
- [ ] **T6-4** 送信バッファと、`Connection` への仮の書き込み口
- [ ] **T6-5** 送信バッファからセグメントを切り出して送る
  - SND.NXT から、相手のウィンドウ（`SND.UNA + SND.WND` まで）と MSS（当面 536 固定）に収まる分だけ送り、SND.NXT を進める
- [ ] **T6-6** ESTABLISHED での ACK の処理（Fifth）
  - `SND.UNA < SEG.ACK =< SND.NXT` なら SND.UNA を進める。重複した ACK は無視し、まだ送っていない範囲への ACK には ACK を返して捨てる
  - 送信ウィンドウを更新する（SND.WL1 と SND.WL2 を使う条件）
- [ ] **T6-7** 固定の HTTP レスポンスを返すデモ（main から使う）

## M7: 切断

完了条件: curl が正常に終了し、`ss -tn` に接続が残らない

- [ ] **T7-1** 状態の追加（FIN-WAIT-1, FIN-WAIT-2, CLOSING, TIME-WAIT, CLOSE-WAIT, LAST-ACK）と stringer の再生成
- [ ] **T7-2** 時刻の注入とタイマーの基盤
  - テストで時間を進められるよう、時計を差し替え可能にする。M8 の再送でも使う
- [ ] **T7-3** FIN の受信（Eighth）
  - RCV.NXT を 1 進めて ACK を返す（FIN もシーケンス番号を 1 消費する）
  - 状態ごとの遷移: ESTABLISHED → CLOSE-WAIT、FIN-WAIT-1 → CLOSING、FIN-WAIT-2 → TIME-WAIT
- [ ] **T7-4** 自分から閉じる（Close）
  - FIN を送る: ESTABLISHED → FIN-WAIT-1、CLOSE-WAIT → LAST-ACK
  - 自分の FIN への ACK を受けたときの遷移: FIN-WAIT-1 → FIN-WAIT-2、CLOSING → TIME-WAIT、LAST-ACK → CLOSED
- [ ] **T7-5** TIME-WAIT の 2MSL タイマー
- [ ] **T7-6** 異常時に RST を送って接続を捨てる（Abort）

## M8: 再送

完了条件: `tc netem` でパケットを落としても転送が最後まで終わる（`sch_netem` はこの環境のカーネルにモジュールとして入っている）

- [ ] **T8-1** 再送キュー（送信済みで ACK 待ちのセグメントと、その送信時刻）
- [ ] **T8-2** ACK で再送キューから取り除き、RTO が切れたら先頭を再送する
  - SYN|ACK と FIN も再送の対象になる
- [ ] **T8-3** RTT の計測と RTO の計算（[RFC 6298](https://datatracker.ietf.org/doc/html/rfc6298)）
  - SRTT と RTTVAR の更新式。再送したセグメントでは RTT を測らない（Karn のアルゴリズム）
- [ ] **T8-4** RTO の指数バックオフと、再送回数の上限
- [ ] **T8-5** netem でパケットロスを起こす Taskfile のタスク

## M9: フロー制御

完了条件: 受信側が読み出しを止めても送信側が溢れさせず、読み出しを再開すると転送が続く

- [ ] **T9-1** 受信バッファの空きから RCV.WND を決めて広告する
- [ ] **T9-2** ゼロウィンドウ probe（persist タイマー）
  - 相手のウィンドウが 0 の間、probe を送り続ける（RFC 9293 [3.8.6.1](https://datatracker.ietf.org/doc/html/rfc9293#section-3.8.6.1)）
- [ ] **T9-3** SWS 回避（受信側）と Nagle（送信側）（RFC 9293 [3.8.6.2](https://datatracker.ietf.org/doc/html/rfc9293#section-3.8.6.2)）

## M10: TCP オプション

完了条件: SYN|ACK に MSS と Window Scale が載り、`ss -ti` で相手側がそれを認識している

- [ ] **T10-1** オプションのパースとシリアライズ（kind / length / value）
- [ ] **T10-2** MSS の交換
  - SYN で受け取った MSS から送信サイズを決め、SYN|ACK で自分の MSS を送る
- [ ] **T10-3** Window Scale（[RFC 7323](https://datatracker.ietf.org/doc/html/rfc7323)）
  - SYN のときだけ交換し、それ以降は SEG.WND をシフトして解釈する
- SACK は M13 に回す

## M11: Socket API

完了条件: `Listen` / `Accept` / `Read` / `Write` / `Close` と `Connect` を、テストから goroutine をまたいで使える

- [ ] **T11-1** 並行処理の設計: stack を 1 つの goroutine のイベントループにし、API からはチャネルで要求を送る
- [ ] **T11-2** `Listen` / `Accept`（accept キュー）
- [ ] **T11-3** `Read` / `Write` / `Close` のブロッキング実装（`net.Conn` に寄せる）
- [ ] **T11-4** `Connect`: SYN-SENT の処理（RFC 9293 [3.10.7.3](https://datatracker.ietf.org/doc/html/rfc9293#section-3.10.7.3)）
  - SYN|ACK を受けたら ESTABLISHED。SYN だけ受けたら（同時オープン）SYN-RECEIVED

## M12: 実環境で通信

完了条件: Socket API で書いた HTTP サーバに `curl` が成功し、大きなファイルを送受信してもハッシュが一致する

- [ ] **T12-1** Socket API を使った HTTP サーバのサンプル（`cmd/` の下）
- [ ] **T12-2** 大きなデータの送受信の確認（`nc` やファイル転送）
- [ ] **T12-3** tcpdump の記録から、handshake・再送・切断が RFC どおりに進むことを `task e2e` で検証する

## M13: 発展（まだ分割していない）

- RFC 5961（RST と SYN への challenge ACK）
- 輻輳制御（slow start, congestion avoidance, fast retransmit）
- SACK
- IPv6
- 性能の測定と最適化

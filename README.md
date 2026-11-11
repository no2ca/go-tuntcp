# go-tuntcp

TUN デバイスの上に、Go でユーザー空間の TCP プロトコルスタックを実装するプロジェクトです。TUN デバイスから生の IP パケットを読み書きし、IPv4 と TCP を自前で処理します。いまは 3-way handshake で接続を確立するところまで動きます。

## ディレクトリ構成

```
cmd/tuntcp/         エントリポイント (TUN を開き、80 番を LISTEN してパケットを処理する)
internal/tun/       TUN デバイスの作成 (ioctl)
internal/checksum/  インターネットチェックサム (Sum / Fold)
internal/ipv4/      IPv4 ヘッダのパース・シリアライズ
internal/tcp/       TCP ヘッダのパース・シリアライズ、シーケンス番号の比較、状態と TCB
internal/stack/     接続の管理と、受信セグメントの処理 (RFC 9293 3.10.7)
scripts/            task から呼ぶシェルスクリプト (e2e.sh)
docs/               ロードマップと設計
```

## ドキュメント

- [docs/roadmap.md](docs/roadmap.md): マイルストーン、タスク、現在の状態
- [docs/design.md](docs/design.md): パッケージの構成、パケットの流れ、設計の決まりごと
- [CLAUDE.md](CLAUDE.md): 開発の進め方。実装は AI が主導し、人間はタスクを選び、差分を読んでコミットします

## 環境

- Linux (`/dev/net/tun` を使用)
- Go 1.26+
- [Task](https://taskfile.dev/)

## 実行

TUN デバイスの作成には root 権限が必要になります。

```sh
go build -o go-tuntcp ./cmd/tuntcp
sudo ./go-tuntcp
```

別ターミナルでIPアドレスの割り当てやパケットの送信を行います。root 権限が必要なコマンドは Taskfile の中で `sudo` を付けているので、`task` 自体は `sudo` なしで実行します。

```sh
task setup-interface        # tun0 に 10.10.0.1 を割り当てて up (内部で sudo を実行)
task ping                   # 10.10.0.2 に ping → ICMP が表示される
task curl                   # 10.10.0.2 に curl → 3-way handshake が表示される
```

上の手順を 1 つのターミナルでまとめて実行することもできます。ビルドしたバイナリに `setcap` で `CAP_NET_ADMIN` を付与してバックグラウンドで起動し、IP の割り当てと `curl` を行ってから終了します。

```sh
task e2e
```

## テスト

```sh
task test
task test-verbose
```

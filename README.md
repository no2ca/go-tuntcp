# go-tuntcp

Go でユーザー空間 TCP プロトコルスタックを自作する学習プロジェクトです。TUN デバイスから生の IP パケットを読み書きし、IPv4 / TCP のパース・組み立てを行います。

## ディレクトリ構成

```
cmd/tuntcp/         エントリポイント (TUN を開いてパケットを読み表示する)
internal/tun/       TUN デバイスの作成 (ioctl)
internal/checksum/  インターネットチェックサム (Sum / Fold)
internal/ipv4/      IPv4 ヘッダのパース・シリアライズ
internal/tcp/       TCP ヘッダのパース・チェックサム、状態と TCB
scripts/            task から呼ぶシェルスクリプト (e2e.sh)
docs/               マイルストーンと次のステップの解説
```

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
task curl                   # 10.10.0.2 に curl → TCP SYN が表示される
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

# go-tuntcp

Go でユーザー空間 TCP プロトコルスタックを自作する学習プロジェクトです。TUN デバイスから生の IP パケットを読み書きし、IPv4 / TCP のパース・組み立てを行います。

## 環境

- Linux (`/dev/net/tun` を使用)
- Go 1.26+
- [Task](https://taskfile.dev/)

## 実行

TUN デバイスの作成には root 権限が必要になります。

```sh
go build -o go-tuntcp .
sudo ./go-tuntcp
```

別ターミナルでIPアドレスの割り当てやパケットの送信を行います。

```sh
sudo task setup-interface   # tun0 に 10.10.0.1 を割り当てて up
task ping                   # 10.10.0.2 に ping → ICMP が表示される
task curl                   # 10.10.0.2 に curl → TCP SYN が表示される
```

## テスト

```sh
task test
task test-verbose
```

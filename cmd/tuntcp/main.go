package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os/signal"
	"syscall"
	"time"

	"go-tuntcp/internal/ipv4"
	"go-tuntcp/internal/stack"
	"go-tuntcp/internal/tcp"
	"go-tuntcp/internal/tun"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	name := "tun0"
	dev, err := tun.Create(name)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		dev.Close()
		fmt.Printf("%s closed\n", name)
	}()

	fmt.Printf("created tun interface: %s\n", name)

	ch := make(chan readResult)
	go readLoop(ctx, ch, dev)

	stk := stack.New()
	stk.Listen(80)
	demo := newDemoServer()

	// タイマーの期限は、Stack.HandleTimeouts を呼んだときに調べられる。いまあるタイマーは
	// TIME-WAIT の 2MSL (4 分) だけなので、1 秒おきで足りる。再送タイマー (M9) を入れるときに、
	// 次の期限まで待つ形に変える (M8 のイベントループ)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			stk.HandleTimeouts()
			for _, pkt := range stk.TakeOutgoing() {
				send(dev, pkt)
			}
		case res := <-ch:
			packet := res.buf[:res.n]
			displayRawRx(packet)

			ip, ipPayload, err := ipv4.Parse(packet)
			if err != nil {
				log.Printf("parse ipv4: %v", err)
				continue
			}
			displayIPv4(ip)

			if ip.Protocol != ipv4.ProtoTCP {
				continue
			}

			t, _, tcpPayload, err := tcp.Parse(ip.Src.As4(), ip.Dst.As4(), ipPayload)
			if err != nil {
				log.Printf("parse tcp: %v", err)
				continue
			}
			displayTCP(t)

			stk.Handle(ip, t, tcpPayload)
			local := netip.AddrPortFrom(ip.Dst, t.DstPort)
			remote := netip.AddrPortFrom(ip.Src, t.SrcPort)
			if c := stk.Conn(local, remote); c != nil {
				demo.serve(c)
			}
			for _, pkt := range stk.TakeOutgoing() {
				send(dev, pkt)
			}
		}
	}
}

// demoServer は、HTTP リクエストを受け取ったら決まったレスポンスを返して閉じる仮のデモ (T6-7、T7-7)。
// リクエストの中身は解釈せず、ヘッダの終わり (空行) が届いたら 1 回返す。本文のあるリクエストは扱わない。
// M8 で net.Listener と net.Conn ができたら、http.Serve に置き換えて捨てる (T8-8)。
type demoServer struct {
	pending map[*stack.Connection][]byte // 接続ごとの、まだ応答していないリクエストの断片
}

// demoBody は scripts/e2e-check.sh が curl の受け取った本文と比べる。変えるときは両方を直す
const demoBody = "hello from go-tuntcp\n"

// レスポンスのあとに接続を閉じるので、Connection: close を付ける (RFC 9112 9.6:
// 持続的な接続を扱わないサーバは、すべてのレスポンスで close を送る)
var demoResponse = []byte("HTTP/1.1 200 OK\r\n" +
	"Content-Type: text/plain\r\n" +
	fmt.Sprintf("Content-Length: %d\r\n", len(demoBody)) +
	"Connection: close\r\n" +
	"\r\n" +
	demoBody)

func newDemoServer() *demoServer {
	return &demoServer{pending: make(map[*stack.Connection][]byte)}
}

// serve は、受信セグメントを 1 つ処理するたびに main が呼ぶ。
// 閉じるのは 2 つの場合。レスポンスを書いたあと (こちらが先に閉じる) と、
// リクエストが揃う前に相手が閉じたとき (CLOSE-WAIT。こちらも閉じないと、接続が残り続ける)。
func (d *demoServer) serve(c *stack.Connection) {
	// 送れる状態のときだけ相手をする。Close を呼んだあとは、FIN のやりとりを Stack に任せる
	if c.State != tcp.StateEstablished && c.State != tcp.StateCloseWait {
		return
	}
	buf := make([]byte, 1500)
	for {
		n := c.Read(buf)
		if n == 0 {
			break
		}
		d.pending[c] = append(d.pending[c], buf[:n]...)
	}
	complete := bytes.Contains(d.pending[c], []byte("\r\n\r\n"))
	if !complete && c.State != tcp.StateCloseWait {
		return
	}
	delete(d.pending, c)
	if complete {
		if err := c.Write(demoResponse); err != nil {
			log.Printf("demo: write: %v", err)
		}
	}
	if err := c.Close(); err != nil {
		log.Printf("demo: close: %v", err)
	}
}

func send(dev io.Writer, pkt []byte) {
	if _, err := dev.Write(pkt); err != nil {
		log.Printf("write: %v", err)
		return
	}
	displayRawTx(pkt)

	// 以下は表示のためだけに、送ったパケットをもう一度パースする
	ip, ipPayload, err := ipv4.Parse(pkt)
	if err != nil {
		log.Printf("parse sent ipv4: %v", err)
		return
	}
	displayIPv4(ip)
	t, _, _, err := tcp.Parse(ip.Src.As4(), ip.Dst.As4(), ipPayload)
	if err != nil {
		log.Printf("parse sent tcp: %v", err)
		return
	}
	displayTCP(t)
}

type readResult struct {
	n   int
	buf []byte
}

func readLoop(ctx context.Context, ch chan readResult, tun io.Reader) {
	defer fmt.Printf("Reading packets done. Waiting for an interrupt...")
	buf := make([]byte, 1500)
	for {
		n, err := tun.Read(buf)
		res := bytes.Clone(buf[:n])
		select {
		case ch <- readResult{n: n, buf: res}:
		case <-ctx.Done():
			return
		}

		if err != nil {
			log.Print(err)
			return
		}
	}
}

func displayRawRx(packet []byte) {
	fmt.Printf("========\n")
	fmt.Printf("[RECEIVED] %d bytes: %  x\n", len(packet), packet)
}

func displayRawTx(packet []byte) {
	fmt.Printf("========\n")
	fmt.Printf("[SENT] %d bytes: %  x\n", len(packet), packet)
}

func displayIPv4(ip ipv4.Header) {
	fmt.Printf("Protocol: %v (%s), TTL: %v, Src: %v, Dst: %v\n", ip.Protocol, ip.StringProtocol(), ip.TTL, ip.Src, ip.Dst)
}

func displayTCP(t tcp.Header) {
	fmt.Printf("SrcPort: %v, DstPort: %v, Seq: %v, Ack: %v\n", t.SrcPort, t.DstPort, t.Seq, t.Ack)
	fmt.Printf("DataOffset: %v, Flags: %v (%v)\n", t.DataOffset, t.Flags, t.StringFlags())
}

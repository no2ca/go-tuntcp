// tuntcp は、TUN デバイスの上の自作 TCP スタックで、標準の net/http サーバを動かすサンプル。
// tun0 を作り、80 番で待ち受ける。流れたパケットを標準出力に表示する。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	tuntcp "go-tuntcp"
	"go-tuntcp/internal/ipv4"
	"go-tuntcp/internal/tcp"
	"go-tuntcp/internal/tun"
)

// helloBody は、"/" が返す本文。scripts/e2e-check.sh が curl の受け取った本文と比べる。変えるときは両方を直す
const helloBody = "hello from go-tuntcp\n"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	name := "tun0"
	dev, err := tun.Create(name)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("created tun interface: %s\n", name)

	// パケットの表示は、スタックの API ではなく、デバイスを包んで行う
	// (docs/design.md の「対話で決めた仕様」の「API とパッケージの形」)
	s := tuntcp.New(traced{dev})
	ln, err := s.Listen(80)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	// "/{$}" は "/" だけに当たる。ほかのパスは、net/http が 404 を返す
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, helloBody)
	})
	go func() {
		// 自作の net.Listener を、標準のサーバにそのまま渡す。スタックを止めると Accept が net.ErrClosed を返し、Serve が終わる
		if err := http.Serve(ln, mux); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("http.Serve: %v", err)
		}
	}()

	<-ctx.Done()
	// 残っている接続に RST を送り、デバイスを閉じる
	if err := s.Close(); err != nil {
		log.Printf("close: %v", err)
	}
	fmt.Printf("%s closed\n", name)
}

// traced は、デバイスを包み、読み書きしたパケットを標準出力に表示する。
type traced struct {
	io.ReadWriteCloser
}

func (d traced) Read(p []byte) (int, error) {
	n, err := d.ReadWriteCloser.Read(p)
	if err != nil {
		// スタックを止めたときは、デバイスが閉じて os.ErrClosed が返る。それ以外は、読めなくなったことを残す
		if !errors.Is(err, os.ErrClosed) {
			log.Printf("tun read: %v", err)
		}
		return n, err
	}
	fmt.Print(describe("RECEIVED", p[:n]))
	return n, nil
}

func (d traced) Write(p []byte) (int, error) {
	n, err := d.ReadWriteCloser.Write(p)
	if err != nil {
		log.Printf("tun write: %v", err)
		return n, err
	}
	fmt.Print(describe("SENT", p))
	return n, nil
}

// describe は、パケット 1 つの表示を組み立てる。Read と Write は別々の goroutine から呼ばれるので、
// 1 つのパケットの行が途中で混ざらないよう、まとめて 1 回で出力する。
func describe(dir string, packet []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "========\n[%s] %d bytes: %  x\n", dir, len(packet), packet)
	ip, ipPayload, err := ipv4.Parse(packet)
	if err != nil {
		fmt.Fprintf(&b, "parse ipv4: %v\n", err)
		return b.String()
	}
	fmt.Fprintf(&b, "Protocol: %v (%s), TTL: %v, Src: %v, Dst: %v\n", ip.Protocol, ip.StringProtocol(), ip.TTL, ip.Src, ip.Dst)
	if ip.Protocol != ipv4.ProtoTCP {
		return b.String()
	}
	t, _, _, err := tcp.Parse(ip.Src.As4(), ip.Dst.As4(), ipPayload)
	if err != nil {
		fmt.Fprintf(&b, "parse tcp: %v\n", err)
		return b.String()
	}
	fmt.Fprintf(&b, "SrcPort: %v, DstPort: %v, Seq: %v, Ack: %v\n", t.SrcPort, t.DstPort, t.Seq, t.Ack)
	fmt.Fprintf(&b, "DataOffset: %v, Flags: %v (%v)\n", t.DataOffset, t.Flags, t.StringFlags())
	return b.String()
}

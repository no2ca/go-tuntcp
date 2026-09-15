package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os/signal"
	"syscall"

	"go-tuntcp/internal/ipv4"
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

	for {
		select {
		case <-ctx.Done():
			return
		case res := <-ch:
			displayPacket(res)
		}
	}
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

func displayPacket(res readResult) {
	fmt.Printf("========\n")
	fmt.Printf("received %d bytes: %  x\n", res.n, res.buf[:res.n])

	ipv4hdr, payload, err := ipv4.Parse(res.buf[:res.n])
	if err != nil {
		log.Print(err)
		return
	}

	fmt.Printf("Protocol: %v (%s), TTL: %v, Src: %v, Dst: %v\n", ipv4hdr.Protocol, ipv4hdr.StringProtocol(), ipv4hdr.TTL, ipv4hdr.Src, ipv4hdr.Dst)

	if ipv4hdr.Protocol == ipv4.ProtoTCP {
		hdr, _, _, err := tcp.Parse(ipv4hdr.Src.As4(), ipv4hdr.Dst.As4(), payload)
		if err != nil {
			log.Print(err)
			return
		}
		fmt.Printf("SrcPort: %v, DstPort: %v, Seq: %v, Ack: %v\n", hdr.SrcPort, hdr.DstPort, hdr.Seq, hdr.Ack)
		fmt.Printf("DataOffset: %v, Flags: %v (%v)\n", hdr.DataOffset, hdr.Flags, hdr.StringFlags())
	}
}

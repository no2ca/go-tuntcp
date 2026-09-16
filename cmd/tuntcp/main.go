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

	for {
		select {
		case <-ctx.Done():
			return
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

			reply := stk.Handle(ip, t, tcpPayload)
			if reply == nil {
				continue
			}

			repIP, repIPPayload, err := ipv4.Parse(reply)
			if err != nil {
				log.Printf("parse reply: %v", err)
				continue
			}

			repTCP, _, _, err := tcp.Parse(ip.Src.As4(), ip.Dst.As4(), repIPPayload)

			if _, err := dev.Write(reply); err != nil {
				log.Printf("write: %v", err)
				continue
			}

			displayRawTx(reply)
			displayIPv4(repIP)
			displayTCP(repTCP)
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

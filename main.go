package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	name := "tun0"
	tun, err := createTUN(name)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		tun.Close()
		fmt.Printf("%s closed\n", name)
	}()

	fmt.Printf("created tun interface: %s\n", name)

	ch := make(chan readResult)
	go readLoop(ctx, ch, tun)

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
		select {
		case ch <- readResult{n: n, buf: buf}:
		case <-ctx.Done():
			return
		}

		if err != nil {
			log.Print(err)
			return
		}
	}
}

const (
	PROTO_ICMP = 1
	PROTO_TCP  = 6
	PROTO_UDP  = 17
)

func displayPacket(res readResult) {
	fmt.Printf("========\n")
	fmt.Printf("received %d bytes: %  x\n", res.n, res.buf[:res.n])

	ipv4hdr, payload, err := ParseIPv4Header(res.buf[:res.n])
	if err != nil {
		log.Print(err)
		return
	}

	var proto string
	switch ipv4hdr.Protocol {
	case PROTO_ICMP:
		proto = "ICMP"
	case PROTO_TCP:
		proto = "TCP"
	case PROTO_UDP:
		proto = "UDP"
	default:
		proto = "other"
	}
	fmt.Printf("Protocol: %v (%s), Src: %v, Dst: %v\n", ipv4hdr.Protocol, proto, ipv4hdr.Src, ipv4hdr.Dst)

	if ipv4hdr.Protocol == PROTO_TCP {
		hdr, _, _, err := ParseTCPHeader(ipv4hdr.Src.As4(), ipv4hdr.Dst.As4(), payload)
		if err != nil {
			log.Print(err)
			return
		}
		fmt.Printf("SrcPort: %v, DstPort: %v, Seq: %v, Ack: %v\n", hdr.SrcPort, hdr.DstPort, hdr.Seq, hdr.Ack)
		fmt.Printf("DataOffset: %v, Flags: %v (%v)\n", hdr.DataOffset, hdr.Flags, hdr.StringFlags())
	}
}

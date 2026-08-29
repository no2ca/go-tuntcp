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

func displayPacket(res readResult) {
	fmt.Printf("========\n")
	fmt.Printf("received %d bytes: %  x\n", res.n, res.buf[:res.n])

	hdr, err := ParseIPv4Header(res.buf)
	if err != nil {
		log.Print(err)
	}
	// IPv4: 1=ICMP, 6=TCP, 17=UDP
	var proto string
	switch hdr.Protocol {
	case 1:
		proto = "ICMP"
	case 6:
		proto = "TCP"
	case 17:
		proto = "UDP"
	default:
		proto = "other"
	}
	fmt.Printf("Protocol: %v (%s), Src: %v, Dst: %v\n", hdr.Protocol, proto, hdr.Src, hdr.Dst)
}

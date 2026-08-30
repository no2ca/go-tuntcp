package main

import (
	"context"
	"encoding/binary"
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

	ipv4hdr, payload, err := ParseIPv4Header(res.buf)
	if err != nil {
		log.Print(err)
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
		hdr, err := ParseTCPHeader(payload)
		if err != nil {
			log.Print(err)
		}
		fmt.Printf("SrcPort: %v, DstPort: %v, Seq: %v, Ack: %v\n", hdr.SrcPort, hdr.DstPort, hdr.Seq, hdr.Ack)
		fmt.Printf("DataOffset: %v, Flags: %v\n", hdr.DataOffset, hdr.Flags)
	}
}

// https://datatracker.ietf.org/doc/html/rfc9293#name-header-format
type TCPHeader struct {
	SrcPort    uint16
	DstPort    uint16
	Seq        uint32
	Ack        uint32
	DataOffset uint8

	Flags  uint8
	Window uint16

	Checksum uint16
	Urgent   uint16
}

func ParseTCPHeader(data []byte) (TCPHeader, error) {
	if len(data) < 20 {
		return TCPHeader{}, fmt.Errorf("[TCP] packet too short: %d bytes", len(data))
	}

	var hdr TCPHeader

	hdr.SrcPort = binary.BigEndian.Uint16(data[0:2])
	hdr.DstPort = binary.BigEndian.Uint16(data[2:4])
	hdr.Seq = binary.BigEndian.Uint32(data[4:8])
	hdr.Ack = binary.BigEndian.Uint32(data[8:12])
	hdr.DataOffset = (data[12] >> 4) * 4
	hdr.Flags = data[13]
	hdr.Window = binary.BigEndian.Uint16(data[14:16])
	hdr.Checksum = binary.BigEndian.Uint16(data[16:18])
	hdr.Urgent = binary.BigEndian.Uint16(data[18:20])

	headerLen := int(hdr.DataOffset)
	if headerLen < 20 {
		return TCPHeader{}, fmt.Errorf("[TCP] invalid data offset: %d", headerLen)
	}
	if len(data) < headerLen {
		return TCPHeader{}, fmt.Errorf("[TCP] packet too short for data offset: %d bytes, need %d", len(data), headerLen)
	}

	return hdr, nil
}

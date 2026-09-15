package stack

import (
	"fmt"
	"go-tuntcp/internal/ipv4"
	"go-tuntcp/internal/tcp"
)

// RFC 9293 3.10.7.1 (CLOSED STATE)
func BuildRST(ip ipv4.Header, t tcp.Header, payloadLen int) []byte {
	if t.HasFlag(tcp.FlagRST) {
		return nil
	}

	tcpHdr := tcp.Header{
		SrcPort:    t.DstPort,
		DstPort:    t.SrcPort,
		DataOffset: 5,
	}

	segLen := uint32(payloadLen)
	if t.HasFlag(tcp.FlagSYN) {
		segLen++
	}
	if t.HasFlag(tcp.FlagFIN) {
		segLen++
	}

	switch {
	case t.HasFlag(tcp.FlagACK):
		tcpHdr.Seq = t.Ack
		tcpHdr.Flags = tcp.FlagRST
	default:
		tcpHdr.Seq = 0
		tcpHdr.Ack = t.Seq + segLen
		tcpHdr.Flags = tcp.FlagRST | tcp.FlagACK
	}

	payload := tcpHdr.Serialize(ip.Dst.As4(), ip.Src.As4(), nil)

	ipHdr := ipv4.Header{
		Version:  4,
		IHL:      5,
		TotalLen: 40,
		TTL:      64,
		Protocol: ipv4.ProtoTCP,
		Src:      ip.Dst,
		Dst:      ip.Src,
	}

	packet := ipHdr.Serialize(payload)

	fmt.Printf("========\n")
	fmt.Printf("created %d bytes: %  x\n", len(packet), packet)
	fmt.Printf("Protocol: %v (%s), TTL: %v, Src: %v, Dst: %v\n", ipHdr.Protocol, ipHdr.StringProtocol(), ipHdr.TTL, ipHdr.Src, ipHdr.Dst)

	fmt.Printf("SrcPort: %v, DstPort: %v, Seq: %v, Ack: %v\n", tcpHdr.SrcPort, tcpHdr.DstPort, tcpHdr.Seq, tcpHdr.Ack)
	fmt.Printf("DataOffset: %v, Flags: %v (%v)\n", tcpHdr.DataOffset, tcpHdr.Flags, tcpHdr.StringFlags())

	return packet
}

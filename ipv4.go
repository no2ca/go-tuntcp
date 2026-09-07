package main

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

type IPv4Header struct {
	Version  uint8
	IHL      uint8
	TotalLen uint16
	Protocol uint8
	Checksum uint16
	Src      netip.Addr
	Dst      netip.Addr
}

func (hdr IPv4Header) StringProtocol() string {
	var proto string
	switch hdr.Protocol {
	case PROTO_ICMP:
		proto = "ICMP"
	case PROTO_TCP:
		proto = "TCP"
	case PROTO_UDP:
		proto = "UDP"
	default:
		proto = "other"
	}
	return proto
}

func ParseIPv4Header(buf []byte) (IPv4Header, []byte, error) {
	if len(buf) < 20 {
		return IPv4Header{}, nil, fmt.Errorf("[IPv4] packet too short: %d bytes", len(buf))
	}

	var hdr IPv4Header

	hdr.Version = buf[0] >> 4
	if hdr.Version != 4 {
		return IPv4Header{}, nil, fmt.Errorf("not an IPv4 packet: version=%d", hdr.Version)
	}

	hdr.IHL = buf[0] & 0x0f
	if hdr.IHL < 5 {
		return IPv4Header{}, nil, fmt.Errorf("invalid IHL: %d", hdr.IHL)
	}
	headerLen := int(hdr.IHL) * 4
	if len(buf) < headerLen {
		return IPv4Header{}, nil, fmt.Errorf("packet too short for IHL: %d bytes, need %d", len(buf), headerLen)
	}

	hdr.TotalLen = binary.BigEndian.Uint16(buf[2:4])
	if int(hdr.TotalLen) > len(buf) {
		return IPv4Header{}, nil, fmt.Errorf("total length %d exceeds buffer size %d", hdr.TotalLen, len(buf))
	}

	hdr.Protocol = buf[9]
	hdr.Checksum = binary.BigEndian.Uint16(buf[10:12])
	hdr.Src = netip.AddrFrom4([4]byte(buf[12:16]))
	hdr.Dst = netip.AddrFrom4([4]byte(buf[16:20]))

	if !VerifyIPv4Checksum(buf[:headerLen]) {
		return IPv4Header{}, nil, fmt.Errorf("checksum mismatch")
	}

	payload := buf[headerLen:hdr.TotalLen]
	return hdr, payload, nil
}

// VerifyIPv4Checksum reports whether the header carries a valid checksum.
func VerifyIPv4Checksum(buf []byte) bool {
	return calculateIPv4Checksum(buf) == 0
}

// calculateIPv4Checksum computes the IPv4 header checksum (RFC 791).
func calculateIPv4Checksum(buf []byte) uint16 {
	return Fold(Sum(buf))
}

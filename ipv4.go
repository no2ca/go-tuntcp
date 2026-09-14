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
	ID       uint16
	TTL      uint8
	Protocol uint8
	Checksum uint16
	Src      netip.Addr
	Dst      netip.Addr
}

func (h IPv4Header) StringProtocol() string {
	var proto string
	switch h.Protocol {
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

func (h IPv4Header) Serialize(payload []byte) []byte {
	buf := make([]byte, 20+len(payload))

	// Version, IHL
	buf[0] = h.Version<<4 | h.IHL
	// Total Length
	binary.BigEndian.PutUint16(buf[2:4], h.TotalLen)
	// Identification
	binary.BigEndian.PutUint16(buf[4:6], h.ID)
	// TTL
	buf[8] = h.TTL
	// Protocol
	buf[9] = h.Protocol
	// Source Address
	src := h.Src.As4()
	copy(buf[12:16], src[:])
	// Destination Address
	dst := h.Dst.As4()
	copy(buf[16:20], dst[:])

	// Checksum
	checksum := calculateIPv4Checksum(buf[:20])
	binary.BigEndian.PutUint16(buf[10:12], checksum)

	copy(buf[20:], payload)

	return buf
}

func ParseIPv4Header(buf []byte) (IPv4Header, []byte, error) {
	if len(buf) < 20 {
		return IPv4Header{}, nil, fmt.Errorf("[IPv4] packet too short: %d bytes", len(buf))
	}

	var hdr IPv4Header

	// Version
	hdr.Version = buf[0] >> 4
	if hdr.Version != 4 {
		return IPv4Header{}, nil, fmt.Errorf("not an IPv4 packet: version=%d", hdr.Version)
	}

	// IHL
	hdr.IHL = buf[0] & 0x0f
	if hdr.IHL < 5 {
		return IPv4Header{}, nil, fmt.Errorf("invalid IHL: %d", hdr.IHL)
	}
	headerLen := int(hdr.IHL) * 4
	if len(buf) < headerLen {
		return IPv4Header{}, nil, fmt.Errorf("packet too short for IHL: %d bytes, need %d", len(buf), headerLen)
	}

	// Total Lengh
	hdr.TotalLen = binary.BigEndian.Uint16(buf[2:4])
	if int(hdr.TotalLen) > len(buf) {
		return IPv4Header{}, nil, fmt.Errorf("total length %d exceeds buffer size %d", hdr.TotalLen, len(buf))
	}
	
	// Identification
	hdr.ID = binary.BigEndian.Uint16(buf[4:6])
	// TTL
	hdr.TTL = buf[8]
	// Protocol
	hdr.Protocol = buf[9]
	// Checksum
	hdr.Checksum = binary.BigEndian.Uint16(buf[10:12])
	// Source Address
	hdr.Src = netip.AddrFrom4([4]byte(buf[12:16]))
	// Destination Address
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

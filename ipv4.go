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

func ParseIPv4Header(buf []byte) (IPv4Header, error) {
	if len(buf) < 20 {
		return IPv4Header{}, fmt.Errorf("packet too short: %d bytes", len(buf))
	}

	var hdr IPv4Header

	hdr.Version = buf[0] >> 4
	if hdr.Version != 4 {
		return IPv4Header{}, fmt.Errorf("not an IPv4 packet: version=%d", hdr.Version)
	}
	
	hdr.IHL = buf[0] & 0x0f
	if hdr.IHL < 5 {
		return IPv4Header{}, fmt.Errorf("invalid IHL: %d", hdr.IHL)
	}
	headerLen := int(hdr.IHL) * 4
	if len(buf) < headerLen {
		return IPv4Header{}, fmt.Errorf("packet too short for IHL: %d bytes, need %d", len(buf), headerLen)
	}

	hdr.TotalLen = binary.BigEndian.Uint16(buf[2:4])
	if int(hdr.TotalLen) > len(buf) {
    return IPv4Header{}, fmt.Errorf("total length %d exceeds buffer size %d", hdr.TotalLen, len(buf))
	}

	hdr.Protocol = buf[9]
	hdr.Checksum = binary.BigEndian.Uint16(buf[10:12])
	hdr.Src = netip.AddrFrom4([4]byte(buf[12:16]))
	hdr.Dst = netip.AddrFrom4([4]byte(buf[16:20]))
	
	if calculateChecksum(buf[:headerLen]) != 0 {
		return IPv4Header{}, fmt.Errorf("checksum mismatch")
	}

	return hdr, nil
}

// calculateChecksum computes the IPv4 header checksum (RFC 791).
func calculateChecksum(buf []byte) uint16 {
	var sum uint32
	
	for i := 0; i+1 < len(buf); i += 2 {
		sum += uint32(binary.BigEndian.Uint16((buf[i : i+2])))
	}

	if len(buf)%2 == 1 {
		sum += uint32(buf[len(buf)-1]) << 8
	}
	
	for sum > 0xffff {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	
	return ^uint16(sum)
}

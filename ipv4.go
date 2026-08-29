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
	Src      netip.Addr
	Dst      netip.Addr
}

func ParseIPv4Header(buf []byte) (IPv4Header, error) {
	if len(buf) < 20 {
		return IPv4Header{}, fmt.Errorf("packet too short: %d bytes", len(buf))
	}

	var hdr IPv4Header

	hdr.Version = buf[0] >> 4
	hdr.IHL = buf[0] & 0x0f
	hdr.TotalLen = binary.BigEndian.Uint16(buf[2:4])
	hdr.Protocol = buf[9]
	hdr.Src = netip.AddrFrom4([4]byte(buf[12:16]))
	hdr.Dst = netip.AddrFrom4([4]byte(buf[16:20]))

	return hdr, nil
}

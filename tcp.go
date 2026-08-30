package main

import (
	"encoding/binary"
	"fmt"
)

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

func ParseTCPHeader(data []byte) (TCPHeader, []byte, []byte, error) {
	if len(data) < 20 {
		return TCPHeader{}, nil, nil, fmt.Errorf("[TCP] packet too short: %d bytes", len(data))
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
		return TCPHeader{}, nil, nil, fmt.Errorf("[TCP] invalid data offset: %d", headerLen)
	}
	if len(data) < headerLen {
		return TCPHeader{}, nil, nil, fmt.Errorf("[TCP] packet too short for data offset: %d bytes, need %d", len(data), headerLen)
	}
	
	options := data[20:headerLen]
	payload := data[headerLen:]

	return hdr, options, payload, nil
}

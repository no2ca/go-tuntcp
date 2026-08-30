package main

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// https://datatracker.ietf.org/doc/html/rfc9293#name-header-format
type TCPHeader struct {
	SrcPort    uint16
	DstPort    uint16
	Seq        uint32
	Ack        uint32
	DataOffset uint8

	Flags  Flag
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
	hdr.Flags = Flag(data[13])
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

type Flag uint8

const (
	FlagFIN Flag = 1 << 0
	FlagSYN Flag = 1 << 1
	FlagRST Flag = 1 << 2
	FlagPSH Flag = 1 << 3
	FlagACK Flag = 1 << 4
	FlagURG Flag = 1 << 5
)

func (h *TCPHeader) HasFlag(flag Flag) bool {
	return h.Flags&flag != 0
}

var flagNames = map[Flag]string{
	FlagFIN: "FIN",
	FlagSYN: "SYN",
	FlagRST: "RST",
	FlagPSH: "PSH",
	FlagACK: "ACK",
	FlagURG: "URG",
}

func (h *TCPHeader) StringFlags() string {
	var names []string
	for flag, name := range flagNames {
		if h.HasFlag(flag) {
			names = append(names, name)
		}
	}
	if names == nil {
		return "None"
	}
	return strings.Join(names, " | ")
}

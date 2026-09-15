package tcp

import (
	"encoding/binary"
	"fmt"
	"strings"

	"go-tuntcp/internal/checksum"
	"go-tuntcp/internal/ipv4"
)

type Flag uint8

const (
	FlagFIN Flag = 1 << 0
	FlagSYN Flag = 1 << 1
	FlagRST Flag = 1 << 2
	FlagPSH Flag = 1 << 3
	FlagACK Flag = 1 << 4
	FlagURG Flag = 1 << 5
)

// https://datatracker.ietf.org/doc/html/rfc9293#name-header-format
type Header struct {
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

func (h Header) Serialize(src, dst [4]byte, payload []byte) []byte {
	buf := make([]byte, 20+len(payload))

	binary.BigEndian.PutUint16(buf[0:2], h.SrcPort)
	binary.BigEndian.PutUint16(buf[2:4], h.DstPort)
	binary.BigEndian.PutUint32(buf[4:8], h.Seq)
	binary.BigEndian.PutUint32(buf[8:12], h.Ack)
	buf[12] = h.DataOffset << 4
	buf[13] = byte(h.Flags)
	binary.BigEndian.PutUint16(buf[14:16], h.Window)
	binary.BigEndian.PutUint16(buf[16:18], 0)
	binary.BigEndian.PutUint16(buf[18:20], h.Urgent)

	copy(buf[20:], payload)

	s := calculateChecksum(src, dst, buf)
	binary.BigEndian.PutUint16(buf[16:18], s)

	return buf
}

func Parse(src, dst [4]byte, seg []byte) (Header, []byte, []byte, error) {
	if len(seg) < 20 {
		return Header{}, nil, nil, fmt.Errorf("[TCP] packet too short: %d bytes", len(seg))
	}

	if !VerifyChecksum(src, dst, seg) {
		return Header{}, nil, nil, fmt.Errorf("checksum mismatch")
	}

	var hdr Header

	hdr.SrcPort = binary.BigEndian.Uint16(seg[0:2])
	hdr.DstPort = binary.BigEndian.Uint16(seg[2:4])
	hdr.Seq = binary.BigEndian.Uint32(seg[4:8])
	hdr.Ack = binary.BigEndian.Uint32(seg[8:12])
	hdr.DataOffset = (seg[12] >> 4)
	hdr.Flags = Flag(seg[13])
	hdr.Window = binary.BigEndian.Uint16(seg[14:16])
	hdr.Checksum = binary.BigEndian.Uint16(seg[16:18])
	hdr.Urgent = binary.BigEndian.Uint16(seg[18:20])

	headerLen := int(hdr.DataOffset) * 4
	if headerLen < 20 {
		return Header{}, nil, nil, fmt.Errorf("[TCP] invalid data offset: %d", headerLen)
	}
	if len(seg) < headerLen {
		return Header{}, nil, nil, fmt.Errorf("[TCP] packet too short for data offset: %d bytes, need %d", len(seg), headerLen)
	}

	options := seg[20:headerLen]
	payload := seg[headerLen:]

	return hdr, options, payload, nil
}

// VerifyChecksum reports whether seg carries a valid checksum for the
// given source and destination addresses.
func VerifyChecksum(src, dst [4]byte, tcp []byte) bool {
	return calculateChecksum(src, dst, tcp) == 0
}

// calculateChecksum computes the TCP checksum over the IPv4 pseudo header
// and the segment (RFC 9293).
func calculateChecksum(src, dst [4]byte, tcp []byte) uint16 {
	pseudo := make([]byte, 12)
	copy(pseudo[0:4], src[:])
	copy(pseudo[4:8], dst[:])
	pseudo[8] = 0
	pseudo[9] = ipv4.ProtoTCP
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(len(tcp)))

	return checksum.Fold(checksum.Sum(pseudo) + checksum.Sum(tcp))
}

func (h *Header) HasFlag(flag Flag) bool {
	return h.Flags&flag != 0
}

var flagNames = []struct {
	flag Flag
	name string
}{
	{FlagURG, "URG"},
	{FlagACK, "ACK"},
	{FlagPSH, "PSH"},
	{FlagRST, "RST"},
	{FlagSYN, "SYN"},
	{FlagFIN, "FIN"},
}

func (h *Header) StringFlags() string {
	var names []string
	for _, f := range flagNames {
		if h.HasFlag(f.flag) {
			names = append(names, f.name)
		}
	}
	if names == nil {
		return "None"
	}
	return strings.Join(names, " | ")
}

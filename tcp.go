package main

import (
	"encoding/binary"
	"fmt"
	"strings"
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

func ParseTCPHeader(ipSrc, ipDst [4]byte, seg []byte) (TCPHeader, []byte, []byte, error) {
	if len(seg) < 20 {
		return TCPHeader{}, nil, nil, fmt.Errorf("[TCP] packet too short: %d bytes", len(seg))
	}

	if !VerifyTCPChecksum(ipSrc, ipDst, seg) {
		return TCPHeader{}, nil, nil, fmt.Errorf("checksum mismatch")
	}

	var hdr TCPHeader

	hdr.SrcPort = binary.BigEndian.Uint16(seg[0:2])
	hdr.DstPort = binary.BigEndian.Uint16(seg[2:4])
	hdr.Seq = binary.BigEndian.Uint32(seg[4:8])
	hdr.Ack = binary.BigEndian.Uint32(seg[8:12])
	hdr.DataOffset = (seg[12] >> 4) * 4
	hdr.Flags = Flag(seg[13])
	hdr.Window = binary.BigEndian.Uint16(seg[14:16])
	hdr.Checksum = binary.BigEndian.Uint16(seg[16:18])
	hdr.Urgent = binary.BigEndian.Uint16(seg[18:20])

	headerLen := int(hdr.DataOffset)
	if headerLen < 20 {
		return TCPHeader{}, nil, nil, fmt.Errorf("[TCP] invalid data offset: %d", headerLen)
	}
	if len(seg) < headerLen {
		return TCPHeader{}, nil, nil, fmt.Errorf("[TCP] packet too short for data offset: %d bytes, need %d", len(seg), headerLen)
	}

	options := seg[20:headerLen]
	payload := seg[headerLen:]

	return hdr, options, payload, nil
}

// VerifyTCPChecksum reports whether seg carries a valid checksum for the
// given source and destination addresses.
func VerifyTCPChecksum(src, dst [4]byte, tcp []byte) bool {
	return calculateTCPChecksum(src, dst, tcp) == 0
}

// calculateTCPChecksum computes the TCP checksum over the IPv4 pseudo header
// and the segment (RFC 9293).
func calculateTCPChecksum(src, dst [4]byte, tcp []byte) uint16 {
	pseudo := make([]byte, 12)
	copy(pseudo[0:4], src[:])
	copy(pseudo[4:8], dst[:])
	pseudo[8] = 0
	pseudo[9] = PROTO_TCP
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(len(tcp)))

	return Fold(Sum(pseudo) + Sum(tcp))
}

func (h *TCPHeader) HasFlag(flag Flag) bool {
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

func (h *TCPHeader) StringFlags() string {
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

//go:generate stringer -type=State -linecomment

// https://datatracker.ietf.org/doc/html/rfc9293#section-3.3.2
type State int

const (
	StateClosed      State = iota // CLOSED
	StateListen                   // LISTEN
	StateSynSent                  // SYN-SENT
	StateSynReceived              // SYN-RECEIVED
	StateEstablished              // ESTABLISHED
)

// SendSequenceSpace は RFC 9293 3.3.1 の Send Sequence Variables
type SendSequenceSpace struct {
	UNA uint32 // 未確認の最古のシーケンス番号
	NXT uint32 // 次に送信するシーケンス番号
	WND uint16 // 相手が広告した受信ウィンドウ
	UP  bool   // urgent pointer
	WL1 uint32 // 最後にウィンドウ更新を行ったセグメントの SEG.SEQ
	WL2 uint32 // 同じく SEG.ACK
	ISS uint32 // 初期送信シーケンス番号
}

// RecvSequenceSpace は RFC 9293 3.3.1 の Receive Sequence Variables
type RecvSequenceSpace struct {
	NXT uint32 // 次に受信を期待するシーケンス番号
	WND uint16 // 自分が広告している受信ウィンドウ
	UP  bool   // urgent pointer
	IRS uint32 // 初期受信シーケンス番号
}

type ControlBlock struct {
	State State
	Snd   SendSequenceSpace
	Rcv   RecvSequenceSpace
}

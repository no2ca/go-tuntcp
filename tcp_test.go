package main

import (
	"bytes"
	"testing"
)

var (
	tcpTestSrc = [4]byte{10, 10, 0, 1}
	tcpTestDst = [4]byte{10, 10, 0, 2}
)

// SrcPort 12345 -> DstPort 80, Seq 1, Ack 0, DataOffset 5, SYN, Window 0xffff。
// チェックサム 0x6b42 は擬似ヘッダ (10.10.0.1 -> 10.10.0.2, TCP, len 20) 込みで
// Go 実装とは別に Python で計算した値。
var validTCPHeader = []byte{
	0x30, 0x39, 0x00, 0x50,
	0x00, 0x00, 0x00, 0x01,
	0x00, 0x00, 0x00, 0x00,
	0x50, 0x02, 0xff, 0xff,
	0x6b, 0x42, 0x00, 0x00,
}

var validTCPStruct = TCPHeader{
	SrcPort:    12345,
	DstPort:    80,
	Seq:        1,
	Ack:        0,
	DataOffset: 5,
	Flags:      FlagSYN,
	Window:     0xffff,
	Checksum:   0x6b42,
	Urgent:     0,
}

func TestParseTCPHeader(t *testing.T) {
	hdr, options, payload, err := ParseTCPHeader(tcpTestSrc, tcpTestDst, validTCPHeader)
	if err != nil {
		t.Fatalf("ParseTCPHeader: %v", err)
	}
	if hdr != validTCPStruct {
		t.Errorf("header = %+v, want %+v", hdr, validTCPStruct)
	}
	if len(options) != 0 {
		t.Errorf("options = %x, want empty", options)
	}
	if len(payload) != 0 {
		t.Errorf("payload = %x, want empty", payload)
	}
}

func TestParseTCPHeader_WithPayload(t *testing.T) {
	// 同じヘッダに "hello" を付けたもの。チェックサムはペイロードと擬似ヘッダの長さも含むので変わる。
	seg := append([]byte{}, validTCPHeader...)
	seg[16], seg[17] = 0x27, 0x6b
	seg = append(seg, []byte("hello")...)

	hdr, _, payload, err := ParseTCPHeader(tcpTestSrc, tcpTestDst, seg)
	if err != nil {
		t.Fatalf("ParseTCPHeader: %v", err)
	}
	if hdr.Checksum != 0x276b {
		t.Errorf("Checksum = %#x, want 0x276b", hdr.Checksum)
	}
	if !bytes.Equal(payload, []byte("hello")) {
		t.Errorf("payload = %q, want %q", payload, "hello")
	}
}

func TestParseTCPHeader_WithOptions(t *testing.T) {
	// DataOffset 6 (24 byte): MSS オプション (kind 2, len 4, 1460) を付ける。
	seg := append([]byte{}, validTCPHeader...)
	seg[12] = 0x60
	seg = append(seg, 0x02, 0x04, 0x05, 0xb4)
	seg = append(seg, []byte("data")...)
	// チェックサムはテスト内で再計算する (値の正しさ自体は TestCalculateTCPChecksum で担保)
	seg[16], seg[17] = 0, 0
	cs := calculateTCPChecksum(tcpTestSrc, tcpTestDst, seg)
	seg[16], seg[17] = byte(cs>>8), byte(cs)

	hdr, options, payload, err := ParseTCPHeader(tcpTestSrc, tcpTestDst, seg)
	if err != nil {
		t.Fatalf("ParseTCPHeader: %v", err)
	}
	if hdr.DataOffset != 6 {
		t.Errorf("DataOffset = %d, want 6", hdr.DataOffset)
	}
	if !bytes.Equal(options, []byte{0x02, 0x04, 0x05, 0xb4}) {
		t.Errorf("options = %x, want 020405b4", options)
	}
	if !bytes.Equal(payload, []byte("data")) {
		t.Errorf("payload = %q, want %q", payload, "data")
	}
}

func TestParseTCPHeader_Errors(t *testing.T) {
	mutate := func(i int, b byte) []byte {
		seg := append([]byte{}, validTCPHeader...)
		seg[i] = b
		return seg
	}

	tests := []struct {
		name string
		src  [4]byte
		dst  [4]byte
		seg  []byte
	}{
		{"too short", tcpTestSrc, tcpTestDst, validTCPHeader[:19]},
		{"checksum mismatch", tcpTestSrc, tcpTestDst, mutate(16, 0x00)},
		{"corrupted flags break checksum", tcpTestSrc, tcpTestDst, mutate(13, 0x12)},
		// 擬似ヘッダに含まれる IP アドレスが違えばチェックサムも合わない
		{"wrong pseudo header", [4]byte{10, 10, 0, 9}, tcpTestDst, validTCPHeader},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, _, err := ParseTCPHeader(tt.src, tt.dst, tt.seg); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestParseTCPHeader_DataOffsetErrors(t *testing.T) {
	// DataOffset の検証はチェックサム検証の後なので、チェックサムを合わせ直してから試す。
	withOffset := func(off byte) []byte {
		seg := append([]byte{}, validTCPHeader...)
		seg[12] = off << 4
		seg[16], seg[17] = 0, 0
		cs := calculateTCPChecksum(tcpTestSrc, tcpTestDst, seg)
		seg[16], seg[17] = byte(cs>>8), byte(cs)
		return seg
	}

	tests := []struct {
		name string
		off  byte
	}{
		{"data offset below 5", 4},
		{"data offset exceeds segment", 15},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, _, err := ParseTCPHeader(tcpTestSrc, tcpTestDst, withOffset(tt.off)); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestCalculateTCPChecksum(t *testing.T) {
	seg := append([]byte{}, validTCPHeader...)
	seg[16], seg[17] = 0, 0
	if got := calculateTCPChecksum(tcpTestSrc, tcpTestDst, seg); got != 0x6b42 {
		t.Errorf("calculateTCPChecksum = %#x, want 0x6b42", got)
	}

	// 奇数長セグメント (ペイロード "hello")
	seg = append(seg, []byte("hello")...)
	if got := calculateTCPChecksum(tcpTestSrc, tcpTestDst, seg); got != 0x276b {
		t.Errorf("calculateTCPChecksum (odd length) = %#x, want 0x276b", got)
	}
}

func TestVerifyTCPChecksum(t *testing.T) {
	if !VerifyTCPChecksum(tcpTestSrc, tcpTestDst, validTCPHeader) {
		t.Error("valid segment reported as invalid")
	}
	// 擬似ヘッダの src/dst は足し算なので入れ替えても検出できない (1 の補数和の性質)。
	// 別のアドレスなら検出できる。
	if VerifyTCPChecksum([4]byte{10, 10, 0, 9}, tcpTestDst, validTCPHeader) {
		t.Error("segment with a different source address must not verify")
	}
}

func TestTCPHeader_HasFlag(t *testing.T) {
	h := TCPHeader{Flags: FlagSYN | FlagACK}
	if !h.HasFlag(FlagSYN) || !h.HasFlag(FlagACK) {
		t.Error("SYN and ACK should be set")
	}
	if h.HasFlag(FlagFIN) || h.HasFlag(FlagRST) || h.HasFlag(FlagPSH) || h.HasFlag(FlagURG) {
		t.Error("unexpected flag reported as set")
	}
}

func TestTCPHeader_StringFlags(t *testing.T) {
	tests := []struct {
		flags Flag
		want  string
	}{
		{0, "None"},
		{FlagSYN, "SYN"},
		{FlagSYN | FlagACK, "ACK | SYN"},
		{FlagFIN | FlagPSH | FlagACK, "ACK | PSH | FIN"},
		{FlagURG | FlagACK | FlagPSH | FlagRST | FlagSYN | FlagFIN, "URG | ACK | PSH | RST | SYN | FIN"},
	}
	for _, tt := range tests {
		h := TCPHeader{Flags: tt.flags}
		if got := h.StringFlags(); got != tt.want {
			t.Errorf("StringFlags(%#x) = %q, want %q", tt.flags, got, tt.want)
		}
	}
}

func TestState_String(t *testing.T) {
	tests := []struct {
		state State
		want  string
	}{
		{StateClosed, "CLOSED"},
		{StateListen, "LISTEN"},
		{StateSynSent, "SYN-SENT"},
		{StateSynReceived, "SYN-RECEIVED"},
		{StateEstablished, "ESTABLISHED"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

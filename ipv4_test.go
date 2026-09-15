package main

import (
	"bytes"
	"net/netip"
	"testing"
)

// 10.10.0.1 -> 10.10.0.2, TTL 64, TCP, TotalLen 40 (20 byte ヘッダ + 20 byte ペイロード)。
// チェックサム 0x66b9 は Go 実装とは別に Python で計算した値。
var validIPv4Header = []byte{
	0x45, 0x00, 0x00, 0x28,
	0x00, 0x01, 0x00, 0x00,
	0x40, 0x06, 0x66, 0xb9,
	0x0a, 0x0a, 0x00, 0x01,
	0x0a, 0x0a, 0x00, 0x02,
}

var validIPv4Struct = IPv4Header{
	Version:  4,
	IHL:      5,
	TotalLen: 40,
	ID:       1,
	TTL:      64,
	Protocol: PROTO_TCP,
	Checksum: 0x66b9,
	Src:      netip.MustParseAddr("10.10.0.1"),
	Dst:      netip.MustParseAddr("10.10.0.2"),
}

func TestParseIPv4Header(t *testing.T) {
	payload := bytes.Repeat([]byte{0xab}, 20)
	pkt := append(append([]byte{}, validIPv4Header...), payload...)

	hdr, got, err := ParseIPv4Header(pkt)
	if err != nil {
		t.Fatalf("ParseIPv4Header: %v", err)
	}
	if hdr != validIPv4Struct {
		t.Errorf("header = %+v, want %+v", hdr, validIPv4Struct)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload = %x, want %x", got, payload)
	}
}

func TestParseIPv4Header_TrailingBytesBeyondTotalLen(t *testing.T) {
	// TUN の読み出しバッファは TotalLen より長いことがある。
	// ペイロードは TotalLen までで切られるべき。
	payload := bytes.Repeat([]byte{0xab}, 20)
	pkt := append(append([]byte{}, validIPv4Header...), payload...)
	pkt = append(pkt, 0xff, 0xff, 0xff)

	_, got, err := ParseIPv4Header(pkt)
	if err != nil {
		t.Fatalf("ParseIPv4Header: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload = %x, want %x", got, payload)
	}
}

func TestParseIPv4Header_Errors(t *testing.T) {
	mutate := func(i int, b byte) []byte {
		pkt := append([]byte{}, validIPv4Header...)
		pkt = append(pkt, bytes.Repeat([]byte{0xab}, 20)...)
		pkt[i] = b
		return pkt
	}

	tests := []struct {
		name string
		pkt  []byte
	}{
		{"too short", validIPv4Header[:19]},
		{"version 6", mutate(0, 0x65)},
		{"IHL below 5", mutate(0, 0x44)},
		{"IHL exceeds buffer", mutate(0, 0x4f)},
		{"total length exceeds buffer", mutate(2, 0xff)},
		{"checksum mismatch", mutate(10, 0x00)},
		{"corrupted TTL breaks checksum", mutate(8, 0x3f)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := ParseIPv4Header(tt.pkt); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestVerifyIPv4Checksum(t *testing.T) {
	if !VerifyIPv4Checksum(validIPv4Header) {
		t.Error("valid header reported as invalid")
	}
	bad := append([]byte{}, validIPv4Header...)
	bad[19] ^= 0x01
	if VerifyIPv4Checksum(bad) {
		t.Error("corrupted header reported as valid")
	}
}

func TestIPv4Header_Serialize(t *testing.T) {
	payload := bytes.Repeat([]byte{0xab}, 20)
	want := append(append([]byte{}, validIPv4Header...), payload...)

	h := validIPv4Struct
	h.Checksum = 0 // Serialize 側で計算されるので入力値は無視されるはず

	got := h.Serialize(payload)
	if !bytes.Equal(got, want) {
		t.Errorf("Serialize() =\n%x\nwant\n%x", got, want)
	}
	if !VerifyIPv4Checksum(got[:20]) {
		t.Error("serialized header has invalid checksum")
	}
}

func TestIPv4Header_Serialize_IgnoresInputChecksum(t *testing.T) {
	h := validIPv4Struct
	h.Checksum = 0xdead
	got := h.Serialize(nil)
	if !VerifyIPv4Checksum(got) {
		t.Error("checksum must be recomputed, not copied from the struct")
	}
}

func TestIPv4Header_RoundTrip(t *testing.T) {
	payload := []byte("hello")
	h := IPv4Header{
		Version:  4,
		IHL:      5,
		TotalLen: uint16(20 + len(payload)),
		ID:       0xbeef,
		TTL:      1,
		Protocol: PROTO_UDP,
		Src:      netip.MustParseAddr("192.168.1.1"),
		Dst:      netip.MustParseAddr("8.8.8.8"),
	}

	buf := h.Serialize(payload)
	parsed, gotPayload, err := ParseIPv4Header(buf)
	if err != nil {
		t.Fatalf("ParseIPv4Header: %v", err)
	}

	h.Checksum = parsed.Checksum // Parse はチェックサムを読むので比較のために揃える
	if parsed != h {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", parsed, h)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Errorf("payload = %q, want %q", gotPayload, payload)
	}
}

func TestIPv4Header_StringProtocol(t *testing.T) {
	tests := []struct {
		proto uint8
		want  string
	}{
		{PROTO_ICMP, "ICMP"},
		{PROTO_TCP, "TCP"},
		{PROTO_UDP, "UDP"},
		{99, "other"},
	}
	for _, tt := range tests {
		h := IPv4Header{Protocol: tt.proto}
		if got := h.StringProtocol(); got != tt.want {
			t.Errorf("StringProtocol(%d) = %q, want %q", tt.proto, got, tt.want)
		}
	}
}

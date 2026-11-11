package stack

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

// このファイルは M6 (データの送受信) の振る舞いを検証する。

// ---------------------------------------------------------------------------
// T6-1: ペイロード付きセグメントの組み立て
// ---------------------------------------------------------------------------

func TestBuildSegment_TotalLenIncludesPayload(t *testing.T) {
	// RFC 791 3.1: Total Length はヘッダとデータを合わせたオクテット数。
	// IPv4 ヘッダ 20 + TCP ヘッダ 20 + ペイロード
	tests := []struct {
		name       string
		payloadLen int
	}{
		{"no payload", 0},
		{"1 byte", 1},
		{"default MSS (536)", 536},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := make([]byte, tt.payloadLen)
			for i := range payload {
				payload[i] = byte(i)
			}
			local := netip.AddrPortFrom(ourAddr, localPort)
			remote := netip.AddrPortFrom(peerAddr, peerPort)

			pkt := buildSegment(local, remote, 0, 1, 2, 3, payload)

			want := 40 + tt.payloadLen
			if got := int(binary.BigEndian.Uint16(pkt[2:4])); got != want {
				t.Fatalf("TotalLen = %d, want %d", got, want)
			}
			_, _, got := parseReply(t, pkt)
			if string(got) != string(payload) {
				t.Errorf("payload = %d bytes, want %d bytes (same content)", len(got), len(payload))
			}
		})
	}
}

package stack

import (
	"encoding/binary"
	"net/netip"
	"testing"

	"go-tuntcp/internal/tcp"
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

// ---------------------------------------------------------------------------
// T6-2: 受信バッファ (RFC 9293 3.10.7.4 Seventh、3.10.3 RECEIVE Call)
// ---------------------------------------------------------------------------

// readAll は、受信バッファに貯まっているものを Read で取り出し切る。
func readAll(c *Connection) string {
	var out []byte
	buf := make([]byte, 4)
	for {
		n := c.Read(buf)
		if n == 0 {
			return string(out)
		}
		out = append(out, buf[:n]...)
	}
}

func TestRead_DeliversInOrderData(t *testing.T) {
	// Seventh: 受理したデータは利用者の受信バッファに渡す。続けて届いたデータは後ろにつながる
	s := newTestStack()
	c := establish(t, s)

	if got := readAll(c); got != "" {
		t.Fatalf("Read before data = %q, want empty", got)
	}
	s.Handle(incomingIP(), incomingTCP(tcp.FlagPSH|tcp.FlagACK, clientISN+1, testISS+1), []byte("hello"))
	s.Handle(incomingIP(), incomingTCP(tcp.FlagPSH|tcp.FlagACK, clientISN+6, testISS+1), []byte(" world"))

	if got := readAll(c); got != "hello world" {
		t.Errorf("Read = %q, want %q", got, "hello world")
	}
	if got := readAll(c); got != "" {
		t.Errorf("second Read = %q, want empty (already consumed)", got)
	}
}

func TestRead_PartialRead(t *testing.T) {
	// 3.10.3: 利用者のバッファが小さければ、入る分だけ渡し、残りは次の Read で渡す
	s := newTestStack()
	c := establish(t, s)
	s.Handle(incomingIP(), incomingTCP(tcp.FlagPSH|tcp.FlagACK, clientISN+1, testISS+1), []byte("hello"))

	buf := make([]byte, 3)
	if n := c.Read(buf); string(buf[:n]) != "hel" {
		t.Errorf("first Read = %q, want %q", buf[:n], "hel")
	}
	if n := c.Read(buf); string(buf[:n]) != "lo" {
		t.Errorf("second Read = %q, want %q", buf[:n], "lo")
	}
}

func TestRead_DiscardedDataNotDelivered(t *testing.T) {
	// RCV.NXT から始まらないデータは捨てる (docs/design.md の「対話で決めた仕様」)。
	// 捨てたデータが受信バッファに入ってはいけない
	const rcvNXT = clientISN + 1
	tests := []struct {
		name string
		seq  uint32
	}{
		{"one byte ahead", rcvNXT + 1},
		{"one byte behind (overlapping)", rcvNXT - 1},
		{"beyond receive window", rcvNXT + uint32(defaultWndSize)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := establish(t, s)
			s.Handle(incomingIP(), incomingTCP(tcp.FlagPSH|tcp.FlagACK, tt.seq, testISS+1), []byte("hello"))
			if got := readAll(c); got != "" {
				t.Errorf("Read = %q, want empty", got)
			}
		})
	}
}

func TestRead_ClientSeqWrapsAround(t *testing.T) {
	// 相手の ISS = 0xfffffffd なら、データ 5 バイトは 0xfffffffe から 2 までを使い、2^32 をまたぐ
	var isn = uint32(0xfffffffd)
	s := newTestStack()
	s.Handle(incomingIP(), incomingTCP(tcp.FlagSYN, isn, 0), nil)
	s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, isn+1, testISS+1), nil)
	c := s.conns[testKey()]
	if c == nil {
		t.Fatal("connection not registered")
	}
	s.Handle(incomingIP(), incomingTCP(tcp.FlagPSH|tcp.FlagACK, isn+1, testISS+1), []byte("hello"))
	s.Handle(incomingIP(), incomingTCP(tcp.FlagPSH|tcp.FlagACK, isn+6, testISS+1), []byte("!"))
	if got := readAll(c); got != "hello!" {
		t.Errorf("Read = %q, want %q", got, "hello!")
	}
}

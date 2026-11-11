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

// ---------------------------------------------------------------------------
// T6-3〜T6-5: 送信キュー、送信バッファ、セグメントの切り出し
// (RFC 9293 3.10.2 SEND Call、3.9.1.2、3.7.1)
// ---------------------------------------------------------------------------

type sentSegment struct {
	hdr     tcp.Header
	payload []byte
}

// takeSent は、送信キューに積まれたパケットをすべて取り出してパースする。
func takeSent(t *testing.T, s *Stack) []sentSegment {
	t.Helper()
	var out []sentSegment
	for _, pkt := range s.TakeOutgoing() {
		th, payload := mustReply(t, pkt)
		out = append(out, sentSegment{th, payload})
	}
	return out
}

// establishWithWindow は handshake を完了させる。最後の ACK で相手の受信ウィンドウを wnd と広告する。
func establishWithWindow(t *testing.T, s *Stack, wnd uint16) *Connection {
	t.Helper()
	c := sendSYN(t, s)
	ack := incomingTCP(tcp.FlagACK, clientISN+1, testISS+1)
	ack.Window = wnd
	if reply := s.Handle(incomingIP(), ack, nil); reply != nil {
		t.Fatalf("reply to handshake ACK = %x, want nil", reply)
	}
	checkState(t, c, tcp.StateEstablished)
	return c
}

func repeat(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a' + byte(i%26)
	}
	return b
}

func TestWrite_Established_SendsSegment(t *testing.T) {
	// 3.10.2 ESTABLISHED: バッファを切り出して、ACK を相乗りさせて送る (ACK = RCV.NXT)。
	// 3.9.1.2 MUST-61: 送り切ってキューが空になるセグメントには PSH を立てる
	s := newTestStack()
	c := establish(t, s)

	if err := c.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	segs := takeSent(t, s)
	if len(segs) != 1 {
		t.Fatalf("sent %d segments, want 1", len(segs))
	}
	checkSegment(t, segs[0].hdr, tcp.FlagACK|tcp.FlagPSH, testISS+1, clientISN+1)
	if string(segs[0].payload) != "hello" {
		t.Errorf("payload = %q, want %q", segs[0].payload, "hello")
	}
	if c.Snd.NXT != testISS+6 {
		t.Errorf("Snd.NXT = %d, want %d", c.Snd.NXT, testISS+6)
	}
	if c.Snd.UNA != testISS+1 {
		t.Errorf("Snd.UNA = %d, want %d (not yet acknowledged)", c.Snd.UNA, testISS+1)
	}
	if again := takeSent(t, s); len(again) != 0 {
		t.Errorf("second TakeOutgoing returned %d segments, want 0", len(again))
	}
}

func TestWrite_SegmentsByMSS(t *testing.T) {
	// 3.7.1 MUST-15: MSS オプションを受け取っていなければ、送信 MSS は 536 とみなす。
	// PSH は最後のセグメントにだけ立てる (MUST-61)
	tests := []struct {
		name     string
		n        int
		wantLens []int
	}{
		{"exactly MSS", 536, []int{536}},
		{"MSS + 1", 537, []int{536, 1}},
		{"2 * MSS + 1", 1073, []int{536, 536, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := establish(t, s)
			data := repeat(tt.n)
			if err := c.Write(data); err != nil {
				t.Fatalf("Write: %v", err)
			}
			segs := takeSent(t, s)
			if len(segs) != len(tt.wantLens) {
				t.Fatalf("sent %d segments, want %d", len(segs), len(tt.wantLens))
			}
			seq, off := testISS+1, 0
			for i, sg := range segs {
				wantFlags := tcp.FlagACK
				if i == len(segs)-1 {
					wantFlags |= tcp.FlagPSH
				}
				checkSegment(t, sg.hdr, wantFlags, seq, clientISN+1)
				if len(sg.payload) != tt.wantLens[i] {
					t.Errorf("segment %d: len = %d, want %d", i, len(sg.payload), tt.wantLens[i])
				}
				if string(sg.payload) != string(data[off:off+len(sg.payload)]) {
					t.Errorf("segment %d: payload does not match data[%d:]", i, off)
				}
				seq += uint32(len(sg.payload))
				off += len(sg.payload)
			}
			if c.Snd.NXT != testISS+1+uint32(tt.n) {
				t.Errorf("Snd.NXT = %d, want %d", c.Snd.NXT, testISS+1+uint32(tt.n))
			}
		})
	}
}

func TestWrite_LimitedBySendWindow(t *testing.T) {
	// 送ってよいのは SND.UNA + SND.WND の手前まで (RFC 9293 3.3.1 の Send Sequence Space の図)。
	// 残りは送信バッファに残すので、送ったセグメントのあとにもキューにデータがあり、PSH は立てない
	tests := []struct {
		name      string
		wnd       uint16
		n         int
		wantLen   int
		wantFlags tcp.Flag
	}{
		{"window == data", 5, 5, 5, tcp.FlagACK | tcp.FlagPSH},
		{"window == data - 1", 4, 5, 4, tcp.FlagACK},
		{"window 1", 1, 5, 1, tcp.FlagACK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := establishWithWindow(t, s, tt.wnd)
			if err := c.Write(repeat(tt.n)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			segs := takeSent(t, s)
			if len(segs) != 1 {
				t.Fatalf("sent %d segments, want 1", len(segs))
			}
			checkSegment(t, segs[0].hdr, tt.wantFlags, testISS+1, clientISN+1)
			if len(segs[0].payload) != tt.wantLen {
				t.Errorf("len = %d, want %d", len(segs[0].payload), tt.wantLen)
			}
			if c.Snd.NXT != testISS+1+uint32(tt.wantLen) {
				t.Errorf("Snd.NXT = %d, want %d", c.Snd.NXT, testISS+1+uint32(tt.wantLen))
			}
		})
	}
}

func TestWrite_WindowFull_SendsNothing(t *testing.T) {
	// ウィンドウを使い切ったら、次の Write のデータは送らずにバッファに残す
	s := newTestStack()
	c := establishWithWindow(t, s, 5)
	c.Write([]byte("hello"))
	takeSent(t, s)

	c.Write([]byte("!"))
	if segs := takeSent(t, s); len(segs) != 0 {
		t.Errorf("sent %d segments with a full window, want 0", len(segs))
	}
	if c.Snd.NXT != testISS+6 {
		t.Errorf("Snd.NXT = %d, want %d", c.Snd.NXT, testISS+6)
	}
}

func TestWrite_ZeroWindow_SendsNothing(t *testing.T) {
	// 相手のウィンドウが 0 なら送らない。ゼロウィンドウ probe は M10 (T10-2)
	s := newTestStack()
	c := establishWithWindow(t, s, 0)
	c.Write([]byte("hello"))
	if segs := takeSent(t, s); len(segs) != 0 {
		t.Errorf("sent %d segments to a zero window, want 0", len(segs))
	}
}

func TestWrite_SynReceived_QueuedUntilEstablished(t *testing.T) {
	// 3.10.2 SYN-RECEIVED: データは ESTABLISHED になってから送るためにキューに積む。
	// handshake の ACK で ESTABLISHED になったら、続けて送る
	s := newTestStack()
	c := sendSYN(t, s)
	if err := c.Write([]byte("early")); err != nil {
		t.Fatalf("Write in SYN-RECEIVED: %v", err)
	}
	if segs := takeSent(t, s); len(segs) != 0 {
		t.Fatalf("sent %d segments in SYN-RECEIVED, want 0", len(segs))
	}

	s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, testISS+1), nil)
	segs := takeSent(t, s)
	if len(segs) != 1 {
		t.Fatalf("sent %d segments after ESTABLISHED, want 1", len(segs))
	}
	// SYN が使った ISS の次から、データの先頭が始まる
	checkSegment(t, segs[0].hdr, tcp.FlagACK|tcp.FlagPSH, testISS+1, clientISN+1)
	if string(segs[0].payload) != "early" {
		t.Errorf("payload = %q, want %q", segs[0].payload, "early")
	}
}

func TestWrite_SndNxtWrapsAround(t *testing.T) {
	// ISS = 0xfffffffe なら、データは 0xffffffff から始まり、SND.NXT は 2^32 をまたいで 4 になる
	s := newTestStack()
	s.ISS = func() uint32 { return 0xfffffffe }
	s.Handle(incomingIP(), incomingTCP(tcp.FlagSYN, clientISN, 0), nil)
	s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, 0xffffffff), nil)
	c := s.conns[testKey()]
	if c == nil {
		t.Fatal("connection not registered")
	}
	c.Write([]byte("hello"))
	segs := takeSent(t, s)
	if len(segs) != 1 {
		t.Fatalf("sent %d segments, want 1", len(segs))
	}
	checkSegment(t, segs[0].hdr, tcp.FlagACK|tcp.FlagPSH, 0xffffffff, clientISN+1)
	if c.Snd.NXT != 4 {
		t.Errorf("Snd.NXT = %d, want 4", c.Snd.NXT)
	}
}

func TestWrite_Closed_ReturnsError(t *testing.T) {
	// 3.10.2 CLOSED: "error: connection does not exist"。送信キューには何も積まない
	s := newTestStack()
	c := establish(t, s)
	s.Handle(incomingIP(), incomingTCP(tcp.FlagRST, clientISN+1, 0), nil)
	checkState(t, c, tcp.StateClosed)

	if err := c.Write([]byte("hello")); err == nil {
		t.Error("Write on a closed connection: err = nil, want an error")
	}
	if segs := takeSent(t, s); len(segs) != 0 {
		t.Errorf("sent %d segments on a closed connection, want 0", len(segs))
	}
}

package stack

import (
	"testing"

	"go-tuntcp/internal/tcp"
)

// このファイルは、tuntcp.Conn が使う口 (T8-4、T8-5) の振る舞いを検証する。
// 仕様は docs/design.md の「対話で決めた仕様」の「net.Conn」と、RFC 9293 3.6.1 SHLD-3。

func TestWrite_SendBufferLimit(t *testing.T) {
	// 送信バッファの上限は 65535 バイト。Write は空いている分だけ積み、積めたバイト数を返す。
	// 相手のウィンドウを 0 にして、積んだものが送られても ACK されない (バッファから減らない) ようにする
	tests := []struct {
		name   string
		first  int // 先に積む量
		second int // 続けて書く量
		wantN  int // 2 回目に積める量
	}{
		{"1 つ空いている", 65534, 2, 1},
		{"ちょうど埋まる", 65534, 1, 1},
		{"満杯", 65535, 1, 0},
		{"空のバッファに上限を越えて書く", 0, 65536, 65535},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := establishWithWindow(t, s, 0)
			if n, err := c.Write(repeat(tt.first)); n != tt.first || err != nil {
				t.Fatalf("setup: Write(%d) = (%d, %v)", tt.first, n, err)
			}

			n, err := c.Write(repeat(tt.second))

			if n != tt.wantN || err != nil {
				t.Errorf("Write(%d) = (%d, %v), want (%d, nil)", tt.second, n, err, tt.wantN)
			}
		})
	}
}

func TestWrite_SendBufferFreedByACK(t *testing.T) {
	// ACK されたデータは送信バッファから消え、その分だけまた積める
	s := newTestStack()
	c := establish(t, s)
	if n, err := c.Write(repeat(65535)); n != 65535 || err != nil {
		t.Fatalf("setup: Write = (%d, %v)", n, err)
	}
	s.TakeOutgoing()
	if n, _ := c.Write(repeat(100)); n != 0 {
		t.Fatalf("setup: Write into a full buffer = %d, want 0", n)
	}

	s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, testISS+1+536), nil)
	s.TakeOutgoing()

	if n, err := c.Write(repeat(1000)); n != 536 || err != nil {
		t.Errorf("Write(1000) after 536 bytes were ACKed = (%d, %v), want (536, nil)", n, err)
	}
}

func TestPeerClosed(t *testing.T) {
	// 相手の FIN を受けたあとは true。接続が消えたあとも変わらない (Read が io.EOF を返すのに使う)
	s := newTestStack()
	c := establish(t, s)
	if c.PeerClosed() {
		t.Error("PeerClosed = true in ESTABLISHED, want false")
	}

	th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+1, testISS+1), nil))
	checkSegment(t, th, tcp.FlagACK, testISS+1, clientISN+2)
	if !c.PeerClosed() {
		t.Error("PeerClosed = false after the peer's FIN, want true")
	}

	// 自分も閉じ、FIN が ACK されて CLOSED になったあと
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s.TakeOutgoing()
	s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+2, testISS+2), nil)
	mustState(t, c, tcp.StateClosed)
	if !c.PeerClosed() {
		t.Error("PeerClosed = false after CLOSED, want true")
	}
	if c.Reset() {
		t.Error("Reset = true after a normal close, want false")
	}
}

func TestPeerClosed_FINNotAtRcvNxt(t *testing.T) {
	// 処理しなかった FIN (手前が抜けている) では true にならない
	s := newTestStack()
	c := establish(t, s)

	mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+2, testISS+1), nil))

	if c.PeerClosed() {
		t.Error("PeerClosed = true after a FIN beyond RCV.NXT, want false")
	}
}

func TestReset(t *testing.T) {
	// 相手の RST で消えたときだけ true。自分の Abort では false
	t.Run("相手の RST", func(t *testing.T) {
		s := newTestStack()
		c := establish(t, s)
		if c.Reset() {
			t.Error("Reset = true in ESTABLISHED, want false")
		}
		s.Handle(incomingIP(), incomingTCP(tcp.FlagRST, clientISN+1, 0), nil)
		mustState(t, c, tcp.StateClosed)
		if !c.Reset() {
			t.Error("Reset = false after the peer's RST, want true")
		}
	})
	t.Run("自分の Abort", func(t *testing.T) {
		s := newTestStack()
		c := establish(t, s)
		if err := c.Abort(); err != nil {
			t.Fatalf("Abort: %v", err)
		}
		if c.Reset() {
			t.Error("Reset = true after our own Abort, want false")
		}
	})
	t.Run("受理できない RST", func(t *testing.T) {
		s := newTestStack()
		c := establish(t, s)
		s.Handle(incomingIP(), incomingTCP(tcp.FlagRST, clientISN, 0), nil)
		mustState(t, c, tcp.StateEstablished)
		if c.Reset() {
			t.Error("Reset = true after an unacceptable RST, want false")
		}
	})
}

func TestRelease_NoUnreadData_SendsFIN(t *testing.T) {
	// 読み残しがなければ、RFC の CLOSE と同じ。FIN を送って閉じる
	for _, tt := range []struct {
		name  string
		setup func(*testing.T, *Stack) *Connection
		want  tcp.State
	}{
		{"ESTABLISHED", establish, tcp.StateFinWait1},
		{"CLOSE-WAIT", toCloseWait, tcp.StateLastAck},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := tt.setup(t, s)

			c.Release()

			segs := takeSent(t, s)
			if len(segs) != 1 {
				t.Fatalf("sent %d segments, want 1 (FIN)", len(segs))
			}
			checkSegment(t, segs[0].hdr, tcp.FlagFIN|tcp.FlagACK, testISS+1, c.Rcv.NXT)
			checkState(t, c, tt.want)
		})
	}
}

func TestRelease_UnreadData_SendsRST(t *testing.T) {
	// RFC 9293 3.6.1 SHLD-3: 受け取ったデータを利用者が読まないまま閉じたら、RST を送って
	// データが失われたことを相手に知らせる。1 バイトでも残っていれば RST
	s := newTestStack()
	c := establish(t, s)
	mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagACK|tcp.FlagPSH, clientISN+1, testISS+1), []byte("ab")))
	buf := make([]byte, 1)
	if n := c.Read(buf); n != 1 {
		t.Fatalf("setup: Read = %d, want 1", n)
	}

	c.Release()

	segs := takeSent(t, s)
	if len(segs) != 1 {
		t.Fatalf("sent %d segments, want 1 (RST)", len(segs))
	}
	checkSegment(t, segs[0].hdr, tcp.FlagRST, testISS+1, 0)
	checkState(t, c, tcp.StateClosed)
	if _, ok := s.conns[testKey()]; ok {
		t.Error("connection still in the table")
	}
}

func TestRelease_AllDataRead_SendsFIN(t *testing.T) {
	// 読み切ってあれば FIN (上のテストとの境界)
	s := newTestStack()
	c := establish(t, s)
	mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagACK|tcp.FlagPSH, clientISN+1, testISS+1), []byte("ab")))
	if n := c.Read(make([]byte, 2)); n != 2 {
		t.Fatalf("setup: Read = %d, want 2", n)
	}

	c.Release()

	segs := takeSent(t, s)
	if len(segs) != 1 {
		t.Fatalf("sent %d segments, want 1 (FIN)", len(segs))
	}
	checkSegment(t, segs[0].hdr, tcp.FlagFIN|tcp.FlagACK, testISS+1, clientISN+3)
	checkState(t, c, tcp.StateFinWait1)
}

func TestRelease_DataArrivesAfterwards_SendsRST(t *testing.T) {
	// RFC 9293 3.6.1 SHLD-3 の後半: 閉じたあとに新しいデータが届いたら RST。
	// FIN-WAIT-1 (自分の FIN が ACK される前) と FIN-WAIT-2 (ACK されたあと) の両方
	for _, tt := range []struct {
		name     string
		ackOurFI bool
	}{
		{"FIN-WAIT-1", false},
		{"FIN-WAIT-2", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := establish(t, s)
			c.Release()
			s.TakeOutgoing()
			if tt.ackOurFI {
				s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, testISS+2), nil)
				mustState(t, c, tcp.StateFinWait2)
			}

			s.Handle(incomingIP(), incomingTCP(tcp.FlagACK|tcp.FlagPSH, clientISN+1, testISS+1), []byte("late"))

			segs := takeSent(t, s)
			if len(segs) != 1 {
				t.Fatalf("sent %d segments, want 1 (RST)", len(segs))
			}
			// SND.NXT は、自分の FIN の次
			checkSegment(t, segs[0].hdr, tcp.FlagRST, testISS+2, 0)
			checkState(t, c, tcp.StateClosed)
			if _, ok := s.conns[testKey()]; ok {
				t.Error("connection still in the table")
			}
			if c.Reset() {
				t.Error("Reset = true, want false (we sent the RST)")
			}
		})
	}
}

func TestRelease_NoNewData_NoRST(t *testing.T) {
	// 閉じたあとでも、新しいデータでなければ RST は送らない。
	// データのない ACK、RCV.NXT から始まらないデータ (まるごと捨てて ACK)、相手の FIN
	tests := []struct {
		name      string
		flags     tcp.Flag
		seq       uint32
		payload   string
		wantFlags tcp.Flag // 0 なら返信なし
		wantState tcp.State
	}{
		{"データのない ACK", tcp.FlagACK, clientISN + 1, "", 0, tcp.StateFinWait2},
		{"先に届いた後ろのデータ", tcp.FlagACK, clientISN + 2, "x", tcp.FlagACK, tcp.StateFinWait2},
		{"相手の FIN", tcp.FlagFIN | tcp.FlagACK, clientISN + 1, "", tcp.FlagACK, tcp.StateTimeWait},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStackWithClockOnly()
			c := establish(t, s)
			c.Release()
			s.TakeOutgoing()

			s.Handle(incomingIP(), incomingTCP(tt.flags, tt.seq, testISS+2), []byte(tt.payload))

			segs := takeSent(t, s)
			if tt.wantFlags == 0 {
				if len(segs) != 0 {
					t.Errorf("sent %d segments, want 0", len(segs))
				}
			} else if len(segs) != 1 || segs[0].hdr.Flags != tt.wantFlags {
				t.Errorf("sent %v, want 1 segment with flags %#x", segs, tt.wantFlags)
			}
			checkState(t, c, tt.wantState)
		})
	}
}

func TestClose_DataArrivesAfterwards_StillReceived(t *testing.T) {
	// RFC の CLOSE (Connection.Close) は送る向きだけを閉じる。Release と違い、あとから届いたデータは受け取る
	s := newTestStack()
	c := toFinWait2(t, s)

	th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagACK|tcp.FlagPSH, clientISN+1, testISS+2), []byte("late")))

	checkSegment(t, th, tcp.FlagACK, testISS+2, clientISN+5)
	if got := readAll(c); got != "late" {
		t.Errorf("Read = %q, want %q", got, "late")
	}
}

func TestRelease_Closed_DoesNothing(t *testing.T) {
	// 相手の RST で消えた接続を手放しても、何も送らない
	s := newTestStack()
	c := establish(t, s)
	s.Handle(incomingIP(), incomingTCP(tcp.FlagRST, clientISN+1, 0), nil)
	mustState(t, c, tcp.StateClosed)

	c.Release()

	if segs := takeSent(t, s); len(segs) != 0 {
		t.Errorf("sent %d segments, want 0", len(segs))
	}
}

// newTestStackWithClockOnly は、TIME-WAIT に入る (時計を引く) テストのために、時計を差し替えたスタックを返す。
func newTestStackWithClockOnly() *Stack {
	s, _ := newTestStackWithClock()
	return s
}

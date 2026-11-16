package stack

import (
	"errors"
	"net/netip"
	"testing"

	"go-tuntcp/internal/tcp"
)

// このファイルは T8-3 (accept キュー) の振る舞いを検証する。
// RFC にはない仕様で、docs/design.md の「対話で決めた仕様」の「accept キュー」で決めた。

// handshakeFrom は、相手のポート port から 3-way handshake を流す。
// SYN のあとにできた接続と、最後の ACK のあとに送信キューに積まれたセグメントを返す。
// ISS は newTestStack の testISS。
func handshakeFrom(t *testing.T, s *Stack, port uint16) (*Connection, []tcp.Header) {
	t.Helper()
	syn := incomingTCP(tcp.FlagSYN, clientISN, 0)
	syn.SrcPort = port
	s.Handle(incomingIP(), syn, nil)
	if out := s.TakeOutgoing(); len(out) != 1 {
		t.Fatalf("setup: SYN from port %d queued %d packets, want 1 (SYN|ACK)", port, len(out))
	}
	c := s.conns[connKey{Local: netip.AddrPortFrom(ourAddr, localPort), Remote: netip.AddrPortFrom(peerAddr, port)}]
	if c == nil {
		t.Fatalf("setup: no connection after SYN from port %d", port)
	}

	ack := incomingTCP(tcp.FlagACK, clientISN+1, testISS+1)
	ack.SrcPort = port
	s.Handle(incomingIP(), ack, nil)
	var sent []tcp.Header
	for _, pkt := range s.TakeOutgoing() {
		_, th, _ := parseReply(t, pkt)
		if th.DstPort != port {
			t.Fatalf("setup: reply to port %d, want %d", th.DstPort, port)
		}
		sent = append(sent, th)
	}
	return c, sent
}

func TestAccept_ReturnsEstablishedConnectionsInOrder(t *testing.T) {
	// ESTABLISHED になった順にキューに入り、Accept はその順に 1 つずつ返す。空なら nil
	s := newTestStack()

	if got := s.Accept(localPort); got != nil {
		t.Fatalf("Accept with no connection = %v, want nil", got.Key)
	}
	c1, _ := handshakeFrom(t, s, 50001)
	c2, _ := handshakeFrom(t, s, 50002)

	if got := s.Accept(localPort); got != c1 {
		t.Errorf("first Accept = %p, want the first connection %p", got, c1)
	}
	if got := s.Accept(localPort); got != c2 {
		t.Errorf("second Accept = %p, want the second connection %p", got, c2)
	}
	if got := s.Accept(localPort); got != nil {
		t.Errorf("third Accept = %v, want nil", got.Key)
	}
}

func TestAccept_SynReceived_NotReturned(t *testing.T) {
	// handshake が終わっていない接続は、Accept に出さない
	s := newTestStack()
	sendSYN(t, s)

	if got := s.Accept(localPort); got != nil {
		t.Errorf("Accept = %v, want nil while SYN-RECEIVED", got.Key)
	}
}

func TestAccept_HandshakeACKWithFIN_Queued(t *testing.T) {
	// handshake を終える ACK が FIN も運んでいると、同じセグメントで CLOSE-WAIT まで進む
	// (Fifth で ESTABLISHED、Eighth で CLOSE-WAIT)。この接続もキューに入る
	s := newTestStack()
	c := sendSYN(t, s)
	th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+1, testISS+1), nil))
	checkSegment(t, th, tcp.FlagACK, testISS+1, clientISN+2)
	mustState(t, c, tcp.StateCloseWait)

	if got := s.Accept(localPort); got != c {
		t.Errorf("Accept = %p, want the connection %p", got, c)
	}
}

func TestAccept_QueueFull_AbortsNewConnection(t *testing.T) {
	// キューの上限は 128。128 本目までは入り、129 本目は ESTABLISHED になった時点で
	// <SEQ=SND.NXT><CTL=RST> を送って捨てる (RFC 9293 3.10.5 の Abort)
	s := newTestStack()
	for i := range 128 {
		if _, sent := handshakeFrom(t, s, uint16(50000+i)); len(sent) != 0 {
			t.Fatalf("connection %d: sent %d segments after the handshake, want 0", i+1, len(sent))
		}
	}

	c, sent := handshakeFrom(t, s, 50128)

	if len(sent) != 1 {
		t.Fatalf("connection 129: sent %d segments, want 1 (RST)", len(sent))
	}
	checkSegment(t, sent[0], tcp.FlagRST, testISS+1, 0)
	checkState(t, c, tcp.StateClosed)
	if len(s.conns) != 128 {
		t.Errorf("%d connections in the table, want 128", len(s.conns))
	}

	// 1 つ取り出すと、空きが 1 つできる
	if got := s.Accept(localPort); got == nil {
		t.Fatal("Accept = nil, want a connection")
	}
	if _, sent := handshakeFrom(t, s, 50129); len(sent) != 0 {
		t.Errorf("after Accept: sent %d segments after the handshake, want 0", len(sent))
	}
	if _, sent := handshakeFrom(t, s, 50130); len(sent) != 1 {
		t.Errorf("queue full again: sent %d segments, want 1 (RST)", len(sent))
	}
}

func TestAccept_ConnectionResetWhileQueued_StillReturned(t *testing.T) {
	// キューの中で相手の RST を受けた接続は、CLOSED になってテーブルから消えるが、キューには残る。
	// Accept はそれを返し、利用者は Read や Write のエラーで知る (T8-5)
	s := newTestStack()
	c := establish(t, s)
	if reply := handle(t, s, incomingIP(), incomingTCP(tcp.FlagRST, clientISN+1, 0), nil); reply != nil {
		t.Fatalf("setup: reply to RST = %x, want nil", reply)
	}
	mustState(t, c, tcp.StateClosed)

	if got := s.Accept(localPort); got != c {
		t.Errorf("Accept = %p, want the reset connection %p", got, c)
	}
}

func TestListen_SamePortTwice_ReturnsError(t *testing.T) {
	s := New()

	if err := s.Listen(localPort); err != nil {
		t.Fatalf("first Listen = %v, want nil", err)
	}
	if err := s.Listen(localPort); !errors.Is(err, ErrPortInUse) {
		t.Errorf("second Listen = %v, want ErrPortInUse", err)
	}
	if err := s.Listen(localPort + 1); err != nil {
		t.Errorf("Listen on another port = %v, want nil", err)
	}

	// 閉じたあとは、同じポートでまた待ち受けられる
	s.CloseListener(localPort)
	if err := s.Listen(localPort); err != nil {
		t.Errorf("Listen after CloseListener = %v, want nil", err)
	}
}

func TestCloseListener_AbortsUnacceptedConnections(t *testing.T) {
	// まだ Accept されていない接続 (SYN-RECEIVED のものと、キューの中のもの) は Abort で捨てる。
	// Accept 済みの接続には触らない
	s := newTestStack()
	accepted, _ := handshakeFrom(t, s, 50001)
	if got := s.Accept(localPort); got != accepted {
		t.Fatalf("setup: Accept = %p, want %p", got, accepted)
	}
	queued, _ := handshakeFrom(t, s, 50002)
	syn := incomingTCP(tcp.FlagSYN, clientISN, 0)
	syn.SrcPort = 50003
	s.Handle(incomingIP(), syn, nil)
	s.TakeOutgoing()
	if len(s.conns) != 3 {
		t.Fatalf("setup: %d connections, want 3", len(s.conns))
	}

	s.CloseListener(localPort)

	// RST は 2 つ。宛先は 50002 (キューの中) と 50003 (SYN-RECEIVED)。積む順番は決まっていない
	rst := map[uint16]bool{}
	for _, pkt := range s.TakeOutgoing() {
		_, th, _ := parseReply(t, pkt)
		checkSegment(t, th, tcp.FlagRST, testISS+1, 0)
		rst[th.DstPort] = true
	}
	if len(rst) != 2 || !rst[50002] || !rst[50003] {
		t.Errorf("RST sent to ports %v, want 50002 and 50003", rst)
	}
	checkState(t, queued, tcp.StateClosed)
	checkState(t, accepted, tcp.StateEstablished)
	if len(s.conns) != 1 {
		t.Errorf("%d connections left, want 1 (the accepted one)", len(s.conns))
	}
	if got := s.Accept(localPort); got != nil {
		t.Errorf("Accept after CloseListener = %v, want nil", got.Key)
	}
}

func TestCloseListener_NewSYN_RepliesRST(t *testing.T) {
	// 待ち受けをやめたポートは、閉じたポートと同じ (RFC 9293 3.10.7.1)
	s := newTestStack()
	s.CloseListener(localPort)

	th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagSYN, clientISN, 0), nil))

	checkSegment(t, th, tcp.FlagRST|tcp.FlagACK, 0, clientISN+1)
	if len(s.conns) != 0 {
		t.Errorf("%d connections, want 0", len(s.conns))
	}
}

func TestCloseListener_OtherPort_Untouched(t *testing.T) {
	// ほかのポートの接続と待ち受けには触らない
	s := newTestStack()
	c := establish(t, s)

	s.CloseListener(localPort + 1)

	if segs := takeSent(t, s); len(segs) != 0 {
		t.Errorf("sent %d segments, want 0", len(segs))
	}
	checkState(t, c, tcp.StateEstablished)
	if got := s.Accept(localPort); got != c {
		t.Errorf("Accept = %p, want %p", got, c)
	}
}

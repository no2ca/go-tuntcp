package stack

import (
	"net/netip"
	"testing"

	"go-tuntcp/internal/tcp"
)

// このファイルは docs/step-handshake.md の完了条件を Handle 経由で検証する。
// 内部関数の名前には依存せず、「セグメントを流して返信と TCB を見る」だけにしている。

const (
	testISS   uint32 = 1_000_000 // 自分の ISS (テストでは固定)
	clientISN uint32 = 1000      // 相手 (curl) の ISS
	localPort uint16 = 80        // incomingTCP の DstPort
	peerPort  uint16 = 54321     // incomingTCP の SrcPort
)

func newTestStack() *Stack {
	s := New()
	s.ISS = func() uint32 { return testISS }
	s.Listen(localPort)
	return s
}

func testKey() connKey {
	return connKey{
		Local:  netip.AddrPortFrom(ourAddr, localPort),
		Remote: netip.AddrPortFrom(peerAddr, peerPort),
	}
}

// 返信をパースし、送ってきた相手に向いていることまで確認して TCP ヘッダを返す。
func mustReply(t *testing.T, pkt []byte) (tcp.Header, []byte) {
	t.Helper()
	if pkt == nil {
		t.Fatal("reply = nil, want a packet")
	}
	ip, th, payload := parseReply(t, pkt)
	if ip.Src != ourAddr || ip.Dst != peerAddr {
		t.Errorf("IP %v -> %v, want %v -> %v", ip.Src, ip.Dst, ourAddr, peerAddr)
	}
	if th.SrcPort != localPort || th.DstPort != peerPort {
		t.Errorf("port %d -> %d, want %d -> %d", th.SrcPort, th.DstPort, localPort, peerPort)
	}
	return th, payload
}

func checkSegment(t *testing.T, th tcp.Header, wantFlags tcp.Flag, wantSeq, wantAck uint32) {
	t.Helper()
	if th.Flags != wantFlags {
		t.Errorf("Flags = %s (%#x), want %#x", th.StringFlags(), th.Flags, wantFlags)
	}
	if th.Seq != wantSeq {
		t.Errorf("Seq = %d, want %d", th.Seq, wantSeq)
	}
	if th.Ack != wantAck {
		t.Errorf("Ack = %d, want %d", th.Ack, wantAck)
	}
}

func checkState(t *testing.T, c *Connection, want tcp.State) {
	t.Helper()
	if c.State != want {
		t.Errorf("State = %v, want %v", c.State, want)
	}
}

// SYN を送り、SYN|ACK が返って SYN-RECEIVED の接続ができるところまで進める。
func sendSYN(t *testing.T, s *Stack) *Connection {
	t.Helper()
	th, _ := mustReply(t, s.Handle(incomingIP(), incomingTCP(tcp.FlagSYN, clientISN, 0), nil))
	checkSegment(t, th, tcp.FlagSYN|tcp.FlagACK, testISS, clientISN+1)

	c := s.conns[testKey()]
	if c == nil {
		t.Fatal("connection not registered after SYN")
	}
	checkState(t, c, tcp.StateSynReceived)
	return c
}

// 3-way handshake を完了させ、ESTABLISHED の接続を返す。
func establish(t *testing.T, s *Stack) *Connection {
	t.Helper()
	c := sendSYN(t, s)
	if reply := s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, testISS+1), nil); reply != nil {
		t.Fatalf("reply to handshake ACK = %x, want nil", reply)
	}
	checkState(t, c, tcp.StateEstablished)
	return c
}

// ---------------------------------------------------------------------------
// CLOSED: LISTEN していないポート (RFC 9293 3.10.7.1)
// ---------------------------------------------------------------------------

func TestHandle_ClosedPort(t *testing.T) {
	tests := []struct {
		name      string
		in        tcp.Header
		wantFlags tcp.Flag
		wantSeq   uint32
		wantAck   uint32
	}{
		{"SYN -> RST|ACK", incomingTCP(tcp.FlagSYN, clientISN, 0), tcp.FlagRST | tcp.FlagACK, 0, clientISN + 1},
		{"ACK -> RST", incomingTCP(tcp.FlagACK, clientISN, 5000), tcp.FlagRST, 5000, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			tt.in.DstPort = 8080 // LISTEN していない

			pkt := s.Handle(incomingIP(), tt.in, nil)
			if pkt == nil {
				t.Fatal("reply = nil, want RST")
			}
			_, th, _ := parseReply(t, pkt)
			checkSegment(t, th, tt.wantFlags, tt.wantSeq, tt.wantAck)

			if len(s.conns) != 0 {
				t.Errorf("conns = %d, want 0 (closed port must not create a connection)", len(s.conns))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// LISTEN (RFC 9293 3.10.7.2)
// ---------------------------------------------------------------------------

func TestHandle_Listen(t *testing.T) {
	tests := []struct {
		name string
		in   tcp.Header

		wantReply bool
		wantFlags tcp.Flag
		wantSeq   uint32
		wantAck   uint32
		wantConn  bool
	}{
		{
			// First: RST は無視
			name: "RST is ignored",
			in:   incomingTCP(tcp.FlagRST, clientISN, 0),
		},
		{
			// RST|ACK も RST の検査が先なので無視 (ACK に反応して RST を返してはいけない)
			name: "RST|ACK is ignored",
			in:   incomingTCP(tcp.FlagRST|tcp.FlagACK, clientISN, 5000),
		},
		{
			// Second: ACK には <SEQ=SEG.ACK><CTL=RST>
			name:      "ACK -> RST with Seq = SEG.ACK",
			in:        incomingTCP(tcp.FlagACK, clientISN, 5000),
			wantReply: true,
			wantFlags: tcp.FlagRST,
			wantSeq:   5000,
		},
		{
			// SYN|ACK も ACK の検査が SYN より先
			name:      "SYN|ACK -> RST with Seq = SEG.ACK",
			in:        incomingTCP(tcp.FlagSYN|tcp.FlagACK, clientISN, 5000),
			wantReply: true,
			wantFlags: tcp.FlagRST,
			wantSeq:   5000,
		},
		{
			// Third: SYN には <SEQ=ISS><ACK=SEG.SEQ+1><CTL=SYN,ACK>
			name:      "SYN -> SYN|ACK and connection is created",
			in:        incomingTCP(tcp.FlagSYN, clientISN, 0),
			wantReply: true,
			wantFlags: tcp.FlagSYN | tcp.FlagACK,
			wantSeq:   testISS,
			wantAck:   clientISN + 1,
			wantConn:  true,
		},
		{
			// Fourth: それ以外は捨てる
			name: "no flags is dropped",
			in:   incomingTCP(0, clientISN, 0),
		},
		{
			name: "PSH only is dropped",
			in:   incomingTCP(tcp.FlagPSH, clientISN, 0),
		},
		{
			name: "FIN only is dropped",
			in:   incomingTCP(tcp.FlagFIN, clientISN, 0),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			pkt := s.Handle(incomingIP(), tt.in, nil)

			if !tt.wantReply {
				if pkt != nil {
					t.Errorf("reply = %x, want nil", pkt)
				}
			} else {
				th, payload := mustReply(t, pkt)
				checkSegment(t, th, tt.wantFlags, tt.wantSeq, tt.wantAck)
				if len(payload) != 0 {
					t.Errorf("payload = %x, want empty", payload)
				}
			}

			_, ok := s.conns[testKey()]
			if ok != tt.wantConn {
				t.Errorf("connection registered = %v, want %v", ok, tt.wantConn)
			}
		})
	}
}

func TestHandle_Listen_SYN_InitializesTCB(t *testing.T) {
	s := newTestStack()
	c := sendSYN(t, s)

	// 送信側 (RFC 9293 3.10.7.2: SND.NXT = ISS+1, SND.UNA = ISS)
	if c.Snd.ISS != testISS {
		t.Errorf("Snd.ISS = %d, want %d", c.Snd.ISS, testISS)
	}
	if c.Snd.UNA != testISS {
		t.Errorf("Snd.UNA = %d, want %d (ISS)", c.Snd.UNA, testISS)
	}
	if c.Snd.NXT != testISS+1 {
		t.Errorf("Snd.NXT = %d, want %d (ISS+1: SYN occupies one sequence number)", c.Snd.NXT, testISS+1)
	}

	// 受信側 (RCV.NXT = SEG.SEQ+1, IRS = SEG.SEQ)
	if c.Rcv.IRS != clientISN {
		t.Errorf("Rcv.IRS = %d, want %d", c.Rcv.IRS, clientISN)
	}
	if c.Rcv.NXT != clientISN+1 {
		t.Errorf("Rcv.NXT = %d, want %d", c.Rcv.NXT, clientISN+1)
	}
	if c.Rcv.WND == 0 {
		t.Error("Rcv.WND = 0, peer would never send data")
	}
}

func TestHandle_Listen_SYNACK_AdvertisesWindow(t *testing.T) {
	// SYN|ACK で Window=0 を広告すると相手はデータを送ってこない (zero window)。
	s := newTestStack()
	th, _ := mustReply(t, s.Handle(incomingIP(), incomingTCP(tcp.FlagSYN, clientISN, 0), nil))

	c := s.conns[testKey()]
	if c == nil {
		t.Fatal("connection not registered after SYN")
	}
	if th.Window == 0 {
		t.Error("SYN|ACK Window = 0, want non-zero")
	}
	if th.Window != c.Rcv.WND {
		t.Errorf("SYN|ACK Window = %d, want Rcv.WND = %d", th.Window, c.Rcv.WND)
	}
}

// ---------------------------------------------------------------------------
// SYN-RECEIVED (RFC 9293 3.10.7.4)
// ---------------------------------------------------------------------------

func TestHandle_SynReceived_ACK_Establishes(t *testing.T) {
	s := newTestStack()
	c := establish(t, s)

	// Fifth: SND.UNA <- SEG.ACK, SND.WND <- SEG.WND, WL1 <- SEG.SEQ, WL2 <- SEG.ACK
	if c.Snd.UNA != testISS+1 {
		t.Errorf("Snd.UNA = %d, want %d (our SYN has been acknowledged)", c.Snd.UNA, testISS+1)
	}
	if c.Snd.NXT != testISS+1 {
		t.Errorf("Snd.NXT = %d, want %d", c.Snd.NXT, testISS+1)
	}
	if c.Snd.WND != 0xffff {
		t.Errorf("Snd.WND = %d, want %d (SEG.WND)", c.Snd.WND, 0xffff)
	}
	if c.Snd.WL1 != clientISN+1 {
		t.Errorf("Snd.WL1 = %d, want %d (SEG.SEQ)", c.Snd.WL1, clientISN+1)
	}
	if c.Snd.WL2 != testISS+1 {
		t.Errorf("Snd.WL2 = %d, want %d (SEG.ACK)", c.Snd.WL2, testISS+1)
	}
	if c.Rcv.NXT != clientISN+1 {
		t.Errorf("Rcv.NXT = %d, want %d (pure ACK carries no data)", c.Rcv.NXT, clientISN+1)
	}
}

func TestHandle_SynReceived_BadACK(t *testing.T) {
	// SND.UNA < SEG.ACK <= SND.NXT を満たさない ACK には <SEQ=SEG.ACK><CTL=RST> を返し、状態は変えない。
	tests := []struct {
		name string
		ack  uint32
	}{
		{"SEG.ACK == SND.UNA (our SYN not acknowledged)", testISS},
		{"SEG.ACK < SND.UNA", testISS - 1},
		{"SEG.ACK > SND.NXT (acknowledges what we never sent)", testISS + 2},
		{"SEG.ACK far ahead", testISS + 100000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := sendSYN(t, s)

			th, _ := mustReply(t, s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, tt.ack), nil))
			checkSegment(t, th, tcp.FlagRST, tt.ack, 0)

			checkState(t, c, tcp.StateSynReceived)
			if c.Snd.UNA != testISS {
				t.Errorf("Snd.UNA = %d, want %d (unchanged)", c.Snd.UNA, testISS)
			}
		})
	}
}

func TestHandle_SynReceived_NoACK_Dropped(t *testing.T) {
	// Fifth: ACK ビットの無いセグメントは捨てる (返信も状態変化も無し)
	s := newTestStack()
	c := sendSYN(t, s)

	if reply := s.Handle(incomingIP(), incomingTCP(tcp.FlagPSH, clientISN+1, 0), []byte("x")); reply != nil {
		t.Errorf("reply = %x, want nil", reply)
	}
	checkState(t, c, tcp.StateSynReceived)
}

func TestHandle_SynReceived_OldSeq_ReACKed(t *testing.T) {
	// First: ウィンドウ外 (古い) セグメントには <SEQ=SND.NXT><ACK=RCV.NXT><CTL=ACK> を返して捨てる。
	// RST で接続を壊してはいけない。
	s := newTestStack()
	c := sendSYN(t, s)

	th, _ := mustReply(t, s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN, testISS+1), nil))
	checkSegment(t, th, tcp.FlagACK, testISS+1, clientISN+1)

	checkState(t, c, tcp.StateSynReceived)
}

func TestHandle_SynReceived_RST_ReturnsToListen(t *testing.T) {
	// Second: passive open の SYN-RECEIVED で RST を受けたら LISTEN に戻る = 接続を消す
	s := newTestStack()
	sendSYN(t, s)

	if reply := s.Handle(incomingIP(), incomingTCP(tcp.FlagRST, clientISN+1, 0), nil); reply != nil {
		t.Errorf("reply = %x, want nil", reply)
	}
	if _, ok := s.conns[testKey()]; ok {
		t.Error("connection still registered after RST, want removed")
	}

	// LISTEN に戻ったので、同じ 4-tuple の SYN をまた受け付けられる
	sendSYN(t, s)
}

func TestHandle_SynReceived_RetransmittedSYN(t *testing.T) {
	// 相手が SYN|ACK を取りこぼして SYN を再送してきた (同じ SEG.SEQ)。
	// SEG.SEQ = IRS < RCV.NXT なのでウィンドウ外。接続を壊さないこと。
	s := newTestStack()
	c := sendSYN(t, s)

	reply := s.Handle(incomingIP(), incomingTCP(tcp.FlagSYN, clientISN, 0), nil)

	if _, ok := s.conns[testKey()]; !ok {
		t.Fatal("connection removed by a retransmitted SYN")
	}
	checkState(t, c, tcp.StateSynReceived)
	if c.Rcv.NXT != clientISN+1 {
		t.Errorf("Rcv.NXT = %d, want %d (unchanged)", c.Rcv.NXT, clientISN+1)
	}
	// 返信するなら相手を RCV.NXT に同期させる ACK であること (RST や別の ISS での SYN|ACK は不可)
	if reply != nil {
		th, _ := mustReply(t, reply)
		if !th.HasFlag(tcp.FlagACK) || th.HasFlag(tcp.FlagRST) {
			t.Errorf("Flags = %s, want ACK without RST", th.StringFlags())
		}
		if th.Ack != clientISN+1 {
			t.Errorf("Ack = %d, want %d", th.Ack, clientISN+1)
		}
	}
}

func TestHandle_SynReceived_NewSYNInWindow_ReturnsToListen(t *testing.T) {
	// Fourth: ウィンドウ内に新しい SYN が来るのは異常。passive open なら LISTEN に戻る。
	s := newTestStack()
	sendSYN(t, s)

	s.Handle(incomingIP(), incomingTCP(tcp.FlagSYN, clientISN+10, 0), nil)

	if _, ok := s.conns[testKey()]; ok {
		t.Error("connection still registered after in-window SYN, want removed")
	}
}

// ---------------------------------------------------------------------------
// ESTABLISHED: 受信データに ACK を返す (docs/step-handshake.md 6 節)
// ---------------------------------------------------------------------------

func TestHandle_Established_PureACK_NoReply(t *testing.T) {
	s := newTestStack()
	c := establish(t, s)

	if reply := s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, testISS+1), nil); reply != nil {
		t.Errorf("reply = %x, want nil (nothing to acknowledge)", reply)
	}
	checkState(t, c, tcp.StateEstablished)
}

func TestHandle_Established_Data(t *testing.T) {
	get := []byte("GET / HTTP/1.1\r\nHost: 10.10.0.2\r\n\r\n")
	n := uint32(len(get))

	s := newTestStack()
	c := establish(t, s)

	// 1 通目: 順序どおりのデータ → RCV.NXT が進み、<SEQ=SND.NXT><ACK=RCV.NXT><CTL=ACK> が返る
	th, payload := mustReply(t, s.Handle(incomingIP(), incomingTCP(tcp.FlagPSH|tcp.FlagACK, clientISN+1, testISS+1), get))
	checkSegment(t, th, tcp.FlagACK, testISS+1, clientISN+1+n)
	if len(payload) != 0 {
		t.Errorf("payload = %x, want empty", payload)
	}
	if c.Rcv.NXT != clientISN+1+n {
		t.Errorf("Rcv.NXT = %d, want %d", c.Rcv.NXT, clientISN+1+n)
	}
	if c.Snd.NXT != testISS+1 {
		t.Errorf("Snd.NXT = %d, want %d (a pure ACK does not consume sequence numbers)", c.Snd.NXT, testISS+1)
	}
	checkState(t, c, tcp.StateEstablished)

	// 2 通目: 続きのデータ → さらに進む
	th, _ = mustReply(t, s.Handle(incomingIP(), incomingTCP(tcp.FlagPSH|tcp.FlagACK, clientISN+1+n, testISS+1), []byte("hello")))
	checkSegment(t, th, tcp.FlagACK, testISS+1, clientISN+1+n+5)
	if c.Rcv.NXT != clientISN+1+n+5 {
		t.Errorf("Rcv.NXT = %d, want %d", c.Rcv.NXT, clientISN+1+n+5)
	}
}

func TestHandle_Established_UnexpectedSeq(t *testing.T) {
	// 順序が前後した / 古い / ウィンドウ外のデータ: RCV.NXT は進めず、今の RCV.NXT を ACK で教え直す。
	const rcvNXT = clientISN + 1
	tests := []struct {
		name string
		seq  uint32
	}{
		{"out of order (gap ahead)", rcvNXT + 100},
		{"old duplicate (entirely before RCV.NXT)", rcvNXT - 10},
		{"overlapping (starts before RCV.NXT)", rcvNXT - 2},
		{"beyond receive window", rcvNXT + uint32(defaultWndSize) + 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := establish(t, s)

			th, _ := mustReply(t, s.Handle(incomingIP(), incomingTCP(tcp.FlagPSH|tcp.FlagACK, tt.seq, testISS+1), []byte("hello")))
			checkSegment(t, th, tcp.FlagACK, testISS+1, rcvNXT)

			if c.Rcv.NXT != rcvNXT {
				t.Errorf("Rcv.NXT = %d, want %d (unchanged)", c.Rcv.NXT, rcvNXT)
			}
			checkState(t, c, tcp.StateEstablished)
		})
	}
}

func TestHandle_Established_ZeroWindow(t *testing.T) {
	// RFC 9293 3.10.7.4 First の表、RCV.WND = 0 の行:
	//   SEG.LEN = 0 なら SEG.SEQ == RCV.NXT のときだけ受理
	//   SEG.LEN > 0 なら受理しない
	const rcvNXT = clientISN + 1
	tests := []struct {
		name    string
		seq     uint32
		payload []byte

		wantReply bool // 受理できないときは <SEQ=SND.NXT><ACK=RCV.NXT><CTL=ACK> を返す
	}{
		{"len 0, seq == RCV.NXT: acceptable", rcvNXT, nil, false},
		{"len 0, seq != RCV.NXT: not acceptable", rcvNXT + 1, nil, true},
		{"len > 0: not acceptable", rcvNXT, []byte("x"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := establish(t, s)
			c.Rcv.WND = 0

			reply := s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, tt.seq, testISS+1), tt.payload)
			if !tt.wantReply {
				if reply != nil {
					t.Errorf("reply = %x, want nil", reply)
				}
			} else {
				th, _ := mustReply(t, reply)
				checkSegment(t, th, tcp.FlagACK, testISS+1, rcvNXT)
			}
			if c.Rcv.NXT != rcvNXT {
				t.Errorf("Rcv.NXT = %d, want %d (unchanged)", c.Rcv.NXT, rcvNXT)
			}
		})
	}
}

func TestHandle_Established_RST_Closes(t *testing.T) {
	s := newTestStack()
	establish(t, s)

	if reply := s.Handle(incomingIP(), incomingTCP(tcp.FlagRST, clientISN+1, 0), nil); reply != nil {
		t.Errorf("reply = %x, want nil", reply)
	}
	if _, ok := s.conns[testKey()]; ok {
		t.Error("connection still registered after RST, want removed")
	}
}

// ---------------------------------------------------------------------------
// シーケンス番号の巻き戻り (modulo 2^32)
// ---------------------------------------------------------------------------

func TestHandle_ISSWrapsAround(t *testing.T) {
	// ISS = 0xffffffff なら SND.NXT = 0。相手は ack=0 を返してくるが、
	// SND.UNA (0xffffffff) < 0 <= SND.NXT (0) は modulo 2^32 で成り立たなければならない。
	s := newTestStack()
	s.ISS = func() uint32 { return 0xffffffff }

	th, _ := mustReply(t, s.Handle(incomingIP(), incomingTCP(tcp.FlagSYN, clientISN, 0), nil))
	checkSegment(t, th, tcp.FlagSYN|tcp.FlagACK, 0xffffffff, clientISN+1)

	c := s.conns[testKey()]
	if c == nil {
		t.Fatal("connection not registered after SYN")
	}
	if c.Snd.NXT != 0 {
		t.Fatalf("Snd.NXT = %d, want 0", c.Snd.NXT)
	}

	if reply := s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, 0), nil); reply != nil {
		_, rth, _ := parseReply(t, reply)
		t.Fatalf("reply = %s, want nil (ack=0 must be accepted as ISS+1)", rth.StringFlags())
	}
	checkState(t, c, tcp.StateEstablished)
	if c.Snd.UNA != 0 {
		t.Errorf("Snd.UNA = %d, want 0", c.Snd.UNA)
	}
}

func TestHandle_ClientSeqWrapsAround(t *testing.T) {
	// 相手の ISS = 0xffffffff なら RCV.NXT = 0。そこからデータが来ても正しく進む。
	const isn = uint32(0xffffffff)
	s := newTestStack()

	th, _ := mustReply(t, s.Handle(incomingIP(), incomingTCP(tcp.FlagSYN, isn, 0), nil))
	checkSegment(t, th, tcp.FlagSYN|tcp.FlagACK, testISS, 0)

	if reply := s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, 0, testISS+1), nil); reply != nil {
		t.Fatalf("reply = %x, want nil", reply)
	}
	c := s.conns[testKey()]
	if c == nil {
		t.Fatal("connection not registered")
	}
	checkState(t, c, tcp.StateEstablished)

	th, _ = mustReply(t, s.Handle(incomingIP(), incomingTCP(tcp.FlagPSH|tcp.FlagACK, 0, testISS+1), []byte("hello")))
	checkSegment(t, th, tcp.FlagACK, testISS+1, 5)
	if c.Rcv.NXT != 5 {
		t.Errorf("Rcv.NXT = %d, want 5", c.Rcv.NXT)
	}
}

// ---------------------------------------------------------------------------
// 複数接続: 4-tuple で区別される
// ---------------------------------------------------------------------------

func TestHandle_MultipleConnections(t *testing.T) {
	s := newTestStack()
	iss := testISS
	s.ISS = func() uint32 { iss += 100; return iss }

	// 1 本目 (54321) は ESTABLISHED まで進める
	c1 := establish(t, s)

	// 2 本目 (54322) の SYN。1 本目の状態に影響してはいけない。
	syn2 := incomingTCP(tcp.FlagSYN, 7000, 0)
	syn2.SrcPort = 54322
	th, _ := mustReply(t, s.Handle(incomingIP(), syn2, nil))
	if th.DstPort != 54322 {
		t.Errorf("SYN|ACK DstPort = %d, want 54322", th.DstPort)
	}
	if th.Seq == c1.Snd.ISS {
		t.Errorf("second connection reused ISS %d of the first", th.Seq)
	}
	if th.Ack != 7001 {
		t.Errorf("Ack = %d, want 7001", th.Ack)
	}

	key2 := testKey()
	key2.Remote = netip.AddrPortFrom(peerAddr, 54322)
	c2 := s.conns[key2]
	if c2 == nil {
		t.Fatal("second connection not registered")
	}
	if c1 == c2 {
		t.Fatal("both keys map to the same connection")
	}
	checkState(t, c1, tcp.StateEstablished)
	checkState(t, c2, tcp.StateSynReceived)
	if len(s.conns) != 2 {
		t.Errorf("conns = %d, want 2", len(s.conns))
	}

	// 2 本目に RST → 2 本目だけ消える
	rst2 := incomingTCP(tcp.FlagRST, 7001, 0)
	rst2.SrcPort = 54322
	s.Handle(incomingIP(), rst2, nil)
	if _, ok := s.conns[key2]; ok {
		t.Error("second connection still registered after RST")
	}
	if _, ok := s.conns[testKey()]; !ok {
		t.Error("first connection was removed by RST for another connection")
	}
}

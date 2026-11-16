package tuntcp_test

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	tuntcp "go-tuntcp"
	"go-tuntcp/internal/ipv4"
	"go-tuntcp/internal/tcp"
)

// このファイルは T8-2 (イベントループ) の振る舞いを検証する。
// 仕様は docs/design.md の「対話で決めた仕様」の「API とパッケージの形」(起動と停止)。
// プロトコルの判定そのものは internal/stack のテストが確かめているので、ここで見るのは
// 「デバイスから読んだパケットが Stack に届き、返信がデバイスに書かれる」という配線。

var (
	peerAddr = netip.MustParseAddr("10.10.0.1") // 相手 (カーネルの役)
	ourAddr  = netip.MustParseAddr("10.10.0.2") // 自作スタック
)

const (
	peerPort  uint16 = 54321
	localPort uint16 = 80
	clientISN uint32 = 1000
)

// fakeDev は、メモリの上の TUN デバイス。1 回の Read が IP パケット 1 つを返し、
// 1 回の Write が 1 つを受け取る。Close すると、待っている Read と Write が os.ErrClosed で返る。
type fakeDev struct {
	in     chan []byte // テストが入れたパケット。スタックが Read で読む
	out    chan []byte // スタックが Write で書いたパケット
	closed chan struct{}
	once   sync.Once
}

func newFakeDev() *fakeDev {
	return &fakeDev{
		in:     make(chan []byte),
		out:    make(chan []byte, 1024), // テストが読まなくても、ループが Write で止まらない大きさ
		closed: make(chan struct{}),
	}
}

func (d *fakeDev) Read(p []byte) (int, error) {
	select {
	case pkt := <-d.in:
		return copy(p, pkt), nil
	case <-d.closed:
		return 0, os.ErrClosed
	}
}

func (d *fakeDev) Write(p []byte) (int, error) {
	select {
	case <-d.closed:
		return 0, os.ErrClosed
	default:
	}
	select {
	case d.out <- append([]byte(nil), p...):
		return len(p), nil
	case <-d.closed:
		return 0, os.ErrClosed
	}
}

func (d *fakeDev) Close() error {
	d.once.Do(func() { close(d.closed) })
	return nil
}

func (d *fakeDev) isClosed() bool {
	select {
	case <-d.closed:
		return true
	default:
		return false
	}
}

// inject は、相手から届いたパケットとしてデバイスに入れる。スタックが読むまで待つ。
func (d *fakeDev) inject(t *testing.T, pkt []byte) {
	t.Helper()
	select {
	case d.in <- pkt:
	case <-time.After(time.Second):
		t.Fatal("the stack did not read the packet from the device within 1s")
	}
}

// sent は、スタックがデバイスに書いた次のパケットを、TCP セグメントとしてパースして返す。
// Parse がチェックサムを確かめる。1 秒待っても書かれなければ、テストを止める。
func (d *fakeDev) sent(t *testing.T) tcp.Header {
	t.Helper()
	th, _ := d.sentData(t)
	return th
}

// sentData は、sent と同じことをして、データも返す。
func (d *fakeDev) sentData(t *testing.T) (tcp.Header, []byte) {
	t.Helper()
	select {
	case pkt := <-d.out:
		ip, ipPayload, err := ipv4.Parse(pkt)
		if err != nil {
			t.Fatalf("ipv4.Parse: %v", err)
		}
		if ip.Src != ourAddr || ip.Dst != peerAddr {
			t.Fatalf("IP %v > %v, want %v > %v", ip.Src, ip.Dst, ourAddr, peerAddr)
		}
		th, _, payload, err := tcp.Parse(ip.Src.As4(), ip.Dst.As4(), ipPayload)
		if err != nil {
			t.Fatalf("tcp.Parse: %v", err)
		}
		return th, payload
	case <-time.After(time.Second):
		t.Fatal("the stack wrote no packet to the device within 1s")
		return tcp.Header{}, nil
	}
}

// ipPacket は、相手から自作スタックに宛てた IPv4 パケットを組み立てる。
func ipPacket(proto uint8, payload []byte) []byte {
	h := ipv4.Header{
		Version:  4,
		IHL:      5,
		TotalLen: uint16(20 + len(payload)),
		TTL:      64,
		Protocol: proto,
		Src:      peerAddr,
		Dst:      ourAddr,
	}
	return h.Serialize(payload)
}

// segment は、相手の peerPort から 80 番に宛てた TCP セグメント (IPv4 パケットごと) を組み立てる。
func segment(flags tcp.Flag, seq, ack uint32) []byte {
	return segmentFrom(peerPort, flags, seq, ack, nil)
}

// segmentFrom は、相手のポートとデータを指定して segment と同じものを組み立てる。
func segmentFrom(port uint16, flags tcp.Flag, seq, ack uint32, payload []byte) []byte {
	h := tcp.Header{
		SrcPort:    port,
		DstPort:    localPort,
		Seq:        seq,
		Ack:        ack,
		DataOffset: 5,
		Flags:      flags,
		Window:     0xffff,
	}
	return ipPacket(ipv4.ProtoTCP, h.Serialize(peerAddr.As4(), ourAddr.As4(), payload))
}

// handshake は、相手のポート port から 3-way handshake を流し、自作スタックの ISS を返す。
func handshake(t *testing.T, dev *fakeDev, port uint16) uint32 {
	t.Helper()
	dev.inject(t, segmentFrom(port, tcp.FlagSYN, clientISN, 0, nil))
	th := dev.sent(t)
	if th.Flags != tcp.FlagSYN|tcp.FlagACK || th.DstPort != port {
		t.Fatalf("setup: reply to SYN = %s to port %d, want SYN | ACK to port %d", th.StringFlags(), th.DstPort, port)
	}
	dev.inject(t, segmentFrom(port, tcp.FlagACK, clientISN+1, th.Seq+1, nil))
	dev.sync(t)
	return th.Seq
}

// sync は、これまでに inject したパケットがループに渡ったあとで返る。
// inject は、読み取りの goroutine がパケットを読んだところで返るので、そのままだと、続けて呼ぶ API が
// パケットの処理を追い越すことがある。読めないパケットをもう 1 つ入れると、それが読まれた時点で、
// 前のパケットはループに渡っている (渡すチャネルにバッファがない)。ループは渡された入力を処理し終えてから
// 次の入力を選ぶので、このあとの API の呼び出しは、前のパケットより後になる。
func (d *fakeDev) sync(t *testing.T) {
	t.Helper()
	d.inject(t, []byte{0})
}

// noSent は、スタックがしばらく何も書かないことを確かめる。
func (d *fakeDev) noSent(t *testing.T) {
	t.Helper()
	select {
	case pkt := <-d.out:
		t.Errorf("the stack wrote a packet (%d bytes), want none", len(pkt))
	case <-time.After(50 * time.Millisecond):
	}
}

type acceptResult struct {
	conn net.Conn
	err  error
}

// acceptAsync は、別の goroutine で Accept を呼び、結果をチャネルで返す。
func acceptAsync(l *tuntcp.Listener) <-chan acceptResult {
	ch := make(chan acceptResult, 1)
	go func() {
		c, err := l.Accept()
		ch <- acceptResult{c, err}
	}()
	return ch
}

// wait は、Accept が返るのを 1 秒まで待つ。
func waitAccept(t *testing.T, ch <-chan acceptResult) acceptResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(time.Second):
		t.Fatal("Accept did not return within 1s")
		return acceptResult{}
	}
}

// stillBlocked は、Accept がまだ返っていないことを確かめる。
func stillBlocked(t *testing.T, ch <-chan acceptResult) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("Accept returned (%v, %v), want it to block", r.conn, r.err)
	case <-time.After(50 * time.Millisecond):
	}
}

func listen(t *testing.T, s *tuntcp.Stack) *tuntcp.Listener {
	t.Helper()
	l, err := s.Listen(localPort)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	return l
}

func checkSegment(t *testing.T, th tcp.Header, wantFlags tcp.Flag, wantSeq, wantAck uint32) {
	t.Helper()
	if th.Flags != wantFlags {
		t.Errorf("Flags = %s, want %s", th.StringFlags(), (&tcp.Header{Flags: wantFlags}).StringFlags())
	}
	if th.Seq != wantSeq {
		t.Errorf("Seq = %d, want %d", th.Seq, wantSeq)
	}
	if th.Ack != wantAck {
		t.Errorf("Ack = %d, want %d", th.Ack, wantAck)
	}
}

func TestStack_ClosedPort_RepliesRST(t *testing.T) {
	// New だけでループが動き始める (利用者は Run を呼ばない)。
	// 待ち受けていないポートへの SYN には、RFC 9293 3.10.7.1 の <SEQ=0><ACK=SEG.SEQ+SEG.LEN><CTL=RST,ACK> が返る
	dev := newFakeDev()
	s := tuntcp.New(dev)
	defer s.Close()

	dev.inject(t, segment(tcp.FlagSYN, clientISN, 0))

	checkSegment(t, dev.sent(t), tcp.FlagRST|tcp.FlagACK, 0, clientISN+1)
}

func TestStack_Listen_SYN_RepliesSYNACK(t *testing.T) {
	// Listen は、ループが動いている最中に別の goroutine から呼べる。
	// 返ったあとに届いた SYN には SYN|ACK が返る (RFC 9293 3.10.7.2)
	dev := newFakeDev()
	s := tuntcp.New(dev)
	defer s.Close()

	if _, err := s.Listen(localPort); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	dev.inject(t, segment(tcp.FlagSYN, clientISN, 0))

	th := dev.sent(t)
	checkSegment(t, th, tcp.FlagSYN|tcp.FlagACK, th.Seq, clientISN+1)
}

func TestStack_UnparsablePackets_Skipped(t *testing.T) {
	// IPv4 として読めないもの、TCP でないもの、TCP のチェックサムが合わないものは捨て、
	// ループは止まらない。そのあとの SYN に返信が出て、それがデバイスに書かれる最初のパケットになる
	dev := newFakeDev()
	s := tuntcp.New(dev)
	defer s.Close()

	badChecksum := segment(tcp.FlagSYN, clientISN+500, 0)
	badChecksum[len(badChecksum)-1] ^= 0xff // ウィンドウのあとの緊急ポインタを壊す
	for _, pkt := range [][]byte{
		{0x01, 0x02, 0x03},
		ipPacket(17, []byte{0, 53, 0, 53, 0, 8, 0, 0}), // UDP
		badChecksum,
	} {
		dev.inject(t, pkt)
	}
	dev.inject(t, segment(tcp.FlagSYN, clientISN, 0))

	checkSegment(t, dev.sent(t), tcp.FlagRST|tcp.FlagACK, 0, clientISN+1)
}

func TestStack_Close_AbortsConnectionsThenClosesDevice(t *testing.T) {
	// Close は、残っている接続に <SEQ=SND.NXT><CTL=RST> を送って捨て (RFC 9293 3.10.5)、
	// そのあとでデバイスを閉じる。順番が逆だと RST が書けない
	dev := newFakeDev()
	s := tuntcp.New(dev)
	defer s.Close()

	if _, err := s.Listen(localPort); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	dev.inject(t, segment(tcp.FlagSYN, clientISN, 0))
	iss := dev.sent(t).Seq
	dev.inject(t, segment(tcp.FlagACK, clientISN+1, iss+1))
	if dev.isClosed() {
		t.Fatal("setup: device closed before Close")
	}

	if err := s.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	// SND.NXT は、SYN-RECEIVED でも ESTABLISHED でも ISS+1。上の ACK の処理が Close より
	// あとになっても (その場合は SYN-RECEIVED のまま Abort される)、送る RST は同じになる
	checkSegment(t, dev.sent(t), tcp.FlagRST, iss+1, 0)
	if !dev.isClosed() {
		t.Error("device not closed after Close")
	}
}

func TestStack_Close_Twice(t *testing.T) {
	// 2 回目の Close は、何もせずに 1 回目と同じ結果を返す
	dev := newFakeDev()
	s := tuntcp.New(dev)

	if err := s.Close(); err != nil {
		t.Errorf("first Close = %v, want nil", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	if !dev.isClosed() {
		t.Error("device not closed after Close")
	}
}

func TestStack_Listen_AfterClose(t *testing.T) {
	dev := newFakeDev()
	s := tuntcp.New(dev)
	s.Close()

	if _, err := s.Listen(localPort); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Listen after Close = %v, want net.ErrClosed", err)
	}
}

// ---------------------------------------------------------------------------
// T8-3: net.Listener。仕様は docs/design.md の「対話で決めた仕様」の「accept キュー」。
// キューの上限と順番は internal/stack の TestAccept_* が確かめる。ここで見るのは、待つことと起こすこと
// ---------------------------------------------------------------------------

var _ net.Listener = (*tuntcp.Listener)(nil)

func TestListener_Accept_BlocksUntilHandshakeCompletes(t *testing.T) {
	dev := newFakeDev()
	s := tuntcp.New(dev)
	defer s.Close()
	l := listen(t, s)

	ch := acceptAsync(l)
	stillBlocked(t, ch)

	// SYN だけでは返らない (SYN-RECEIVED)
	dev.inject(t, segment(tcp.FlagSYN, clientISN, 0))
	iss := dev.sent(t).Seq
	stillBlocked(t, ch)

	dev.inject(t, segment(tcp.FlagACK, clientISN+1, iss+1))
	r := waitAccept(t, ch)
	if r.err != nil || r.conn == nil {
		t.Errorf("Accept = (%v, %v), want a connection", r.conn, r.err)
	}
}

func TestListener_Accept_ReturnsQueuedConnection(t *testing.T) {
	// Accept を呼ぶ前に handshake が終わった接続は、キューで待っている
	dev := newFakeDev()
	s := tuntcp.New(dev)
	defer s.Close()
	l := listen(t, s)
	handshake(t, dev, 50001)
	handshake(t, dev, 50002)

	for i := range 2 {
		r := waitAccept(t, acceptAsync(l))
		if r.err != nil || r.conn == nil {
			t.Errorf("Accept %d = (%v, %v), want a connection", i+1, r.conn, r.err)
		}
	}
	stillBlocked(t, acceptAsync(l))
}

func TestListener_Accept_TwoWaiters_BothServed(t *testing.T) {
	// 複数の goroutine が同時に Accept で待てる。接続 1 つにつき 1 つが返る
	dev := newFakeDev()
	s := tuntcp.New(dev)
	defer s.Close()
	l := listen(t, s)
	ch1, ch2 := acceptAsync(l), acceptAsync(l)
	stillBlocked(t, ch1)
	stillBlocked(t, ch2)

	handshake(t, dev, 50001)
	handshake(t, dev, 50002)

	r1, r2 := waitAccept(t, ch1), waitAccept(t, ch2)
	if r1.err != nil || r2.err != nil || r1.conn == nil || r2.conn == nil {
		t.Fatalf("Accept = (%v, %v) and (%v, %v), want two connections", r1.conn, r1.err, r2.conn, r2.err)
	}
	if r1.conn == r2.conn {
		t.Error("both Accept calls returned the same connection")
	}
}

func TestListener_Addr(t *testing.T) {
	// 宛先の IP は見ないので、0.0.0.0 とポートを *net.TCPAddr で返す
	dev := newFakeDev()
	s := tuntcp.New(dev)
	defer s.Close()
	l := listen(t, s)

	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("Addr = %T, want *net.TCPAddr", l.Addr())
	}
	if got := addr.String(); got != "0.0.0.0:80" {
		t.Errorf("Addr = %s, want 0.0.0.0:80", got)
	}
	if got := addr.Network(); got != "tcp" {
		t.Errorf("Network = %s, want tcp", got)
	}
}

func TestListener_Close_UnblocksAccept(t *testing.T) {
	dev := newFakeDev()
	s := tuntcp.New(dev)
	defer s.Close()
	l := listen(t, s)
	ch := acceptAsync(l)
	stillBlocked(t, ch)

	if err := l.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	if r := waitAccept(t, ch); r.conn != nil || !errors.Is(r.err, net.ErrClosed) {
		t.Errorf("blocked Accept = (%v, %v), want net.ErrClosed", r.conn, r.err)
	}
	// 閉じたあとの Accept も、待たずに返る
	if r := waitAccept(t, acceptAsync(l)); r.conn != nil || !errors.Is(r.err, net.ErrClosed) {
		t.Errorf("Accept after Close = (%v, %v), want net.ErrClosed", r.conn, r.err)
	}
	// 2 回目の Close は、標準の net.Listener と同じく net.ErrClosed
	if err := l.Close(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("second Close = %v, want net.ErrClosed", err)
	}
}

func TestListener_Close_StopsListeningAndAbortsUnaccepted(t *testing.T) {
	// 閉じると、Accept されていない接続に RST を送り、以後の SYN は閉じたポートとして扱う
	dev := newFakeDev()
	s := tuntcp.New(dev)
	defer s.Close()
	l := listen(t, s)
	iss := handshake(t, dev, 50001)

	if err := l.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	th := dev.sent(t)
	checkSegment(t, th, tcp.FlagRST, iss+1, 0)
	if th.DstPort != 50001 {
		t.Errorf("RST to port %d, want 50001", th.DstPort)
	}
	dev.inject(t, segmentFrom(50002, tcp.FlagSYN, clientISN, 0, nil))
	checkSegment(t, dev.sent(t), tcp.FlagRST|tcp.FlagACK, 0, clientISN+1)
}

func TestListener_Close_KeepsAcceptedConnection(t *testing.T) {
	// Accept 済みの接続は、Listener を閉じても生きている。データを送ると ACK が返る
	dev := newFakeDev()
	s := tuntcp.New(dev)
	defer s.Close()
	l := listen(t, s)
	iss := handshake(t, dev, 50001)
	if r := waitAccept(t, acceptAsync(l)); r.err != nil || r.conn == nil {
		t.Fatalf("setup: Accept = (%v, %v)", r.conn, r.err)
	}

	if err := l.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	dev.inject(t, segmentFrom(50001, tcp.FlagACK|tcp.FlagPSH, clientISN+1, iss+1, []byte("x")))
	checkSegment(t, dev.sent(t), tcp.FlagACK, iss+1, clientISN+2)
}

func TestStack_Listen_SamePortTwice(t *testing.T) {
	dev := newFakeDev()
	s := tuntcp.New(dev)
	defer s.Close()
	l := listen(t, s)

	if _, err := s.Listen(localPort); !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("second Listen = %v, want EADDRINUSE", err)
	}

	// 閉じたあとは、同じポートでまた待ち受けられる。新しい Listener が接続を受け取る
	l.Close()
	l2, err := s.Listen(localPort)
	if err != nil {
		t.Fatalf("Listen after Close = %v, want nil", err)
	}
	handshake(t, dev, 50001)
	if r := waitAccept(t, acceptAsync(l2)); r.err != nil || r.conn == nil {
		t.Errorf("Accept on the new listener = (%v, %v), want a connection", r.conn, r.err)
	}
}

func TestStack_Close_UnblocksAccept(t *testing.T) {
	// スタックを止めると、すべての Listener が閉じる。キューの中の接続には RST が出る
	dev := newFakeDev()
	s := tuntcp.New(dev)
	l := listen(t, s)
	ch := acceptAsync(l)
	stillBlocked(t, ch)

	s.Close()

	if r := waitAccept(t, ch); r.conn != nil || !errors.Is(r.err, net.ErrClosed) {
		t.Errorf("blocked Accept = (%v, %v), want net.ErrClosed", r.conn, r.err)
	}
	if r := waitAccept(t, acceptAsync(l)); r.conn != nil || !errors.Is(r.err, net.ErrClosed) {
		t.Errorf("Accept after Stack.Close = (%v, %v), want net.ErrClosed", r.conn, r.err)
	}
	if err := l.Close(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Listener.Close after Stack.Close = %v, want net.ErrClosed", err)
	}
}

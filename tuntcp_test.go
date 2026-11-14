package tuntcp_test

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
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
		out:    make(chan []byte, 16),
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
	select {
	case pkt := <-d.out:
		ip, ipPayload, err := ipv4.Parse(pkt)
		if err != nil {
			t.Fatalf("ipv4.Parse: %v", err)
		}
		if ip.Src != ourAddr || ip.Dst != peerAddr {
			t.Fatalf("IP %v > %v, want %v > %v", ip.Src, ip.Dst, ourAddr, peerAddr)
		}
		th, _, _, err := tcp.Parse(ip.Src.As4(), ip.Dst.As4(), ipPayload)
		if err != nil {
			t.Fatalf("tcp.Parse: %v", err)
		}
		return th
	case <-time.After(time.Second):
		t.Fatal("the stack wrote no packet to the device within 1s")
		return tcp.Header{}
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

// segment は、相手から 80 番に宛てた TCP セグメント (IPv4 パケットごと) を組み立てる。
func segment(flags tcp.Flag, seq, ack uint32) []byte {
	h := tcp.Header{
		SrcPort:    peerPort,
		DstPort:    localPort,
		Seq:        seq,
		Ack:        ack,
		DataOffset: 5,
		Flags:      flags,
		Window:     0xffff,
	}
	return ipPacket(ipv4.ProtoTCP, h.Serialize(peerAddr.As4(), ourAddr.As4(), nil))
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

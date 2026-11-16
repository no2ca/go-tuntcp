package tuntcp_test

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	tuntcp "go-tuntcp"
	"go-tuntcp/internal/tcp"
)

// このファイルは T8-4〜T8-7 (net.Conn) の振る舞いを検証する。
// 仕様は docs/roadmap.md の T8-4〜T8-7 と、docs/design.md の「対話で決めた仕様」の「net.Conn」。

var _ net.Conn = (*tuntcp.Conn)(nil)

// peer は、テストが演じる相手側 (カーネルの役)。次に送る SEQ と ACK を覚えている。
type peer struct {
	dev  *fakeDev
	port uint16
	seq  uint32 // 次に送る SEQ
	ack  uint32 // 送る ACK。自作スタックから受け取った分だけ、テストが進める
	iss  uint32 // 自作スタックの ISS
}

// send は、セグメントを 1 つ送り、SEQ を進める。ループに渡るまで待つ。
func (p *peer) send(t *testing.T, flags tcp.Flag, payload string) {
	t.Helper()
	p.dev.inject(t, segmentFrom(p.port, flags, p.seq, p.ack, []byte(payload)))
	p.seq += uint32(len(payload))
	if flags&tcp.FlagFIN != 0 {
		p.seq++
	}
	p.dev.sync(t)
}

// data は、データを送り、返ってきた ACK を確かめる。
func (p *peer) data(t *testing.T, payload string) {
	t.Helper()
	p.send(t, tcp.FlagACK|tcp.FlagPSH, payload)
	checkSegment(t, p.dev.sent(t), tcp.FlagACK, p.ack, p.seq)
}

// fin は、FIN を送り、返ってきた ACK を確かめる。
func (p *peer) fin(t *testing.T) {
	t.Helper()
	p.send(t, tcp.FlagFIN|tcp.FlagACK, "")
	checkSegment(t, p.dev.sent(t), tcp.FlagACK, p.ack, p.seq)
}

// newConn は、Accept 済みの接続と、その相手側を返す。
func newConn(t *testing.T) (net.Conn, *peer) {
	t.Helper()
	dev := newFakeDev()
	s := tuntcp.New(dev)
	t.Cleanup(func() { s.Close() })
	return newConnOn(t, dev, s)
}

func newConnOn(t *testing.T, dev *fakeDev, s *tuntcp.Stack) (net.Conn, *peer) {
	t.Helper()
	l := listen(t, s)
	iss := handshake(t, dev, 50001)
	r := waitAccept(t, acceptAsync(l))
	if r.err != nil || r.conn == nil {
		t.Fatalf("setup: Accept = (%v, %v)", r.conn, r.err)
	}
	return r.conn, &peer{dev: dev, port: 50001, seq: clientISN + 1, ack: iss + 1, iss: iss}
}

type ioResult struct {
	n    int
	err  error
	data []byte // Read が読んだ分
}

func readAsync(c net.Conn, size int) <-chan ioResult {
	ch := make(chan ioResult, 1)
	go func() {
		buf := make([]byte, size)
		n, err := c.Read(buf)
		ch <- ioResult{n, err, buf[:n]}
	}()
	return ch
}

func writeAsync(c net.Conn, p []byte) <-chan ioResult {
	ch := make(chan ioResult, 1)
	go func() {
		n, err := c.Write(p)
		ch <- ioResult{n: n, err: err}
	}()
	return ch
}

// wait は、呼び出しが返るのを 1 秒まで待つ。
func wait(t *testing.T, ch <-chan ioResult) ioResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(time.Second):
		t.Fatal("the call did not return within 1s")
		return ioResult{}
	}
}

// blocked は、呼び出しがまだ返っていないことを確かめる。
func blocked(t *testing.T, ch <-chan ioResult) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("the call returned (%d, %v), want it to block", r.n, r.err)
	case <-time.After(50 * time.Millisecond):
	}
}

// checkOpError は、エラーが標準の net と同じ形 (*net.OpError) で、中身が want であることを確かめる。
func checkOpError(t *testing.T, err error, op string, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
	var oe *net.OpError
	if !errors.As(err, &oe) {
		t.Errorf("err = %T (%v), want *net.OpError", err, err)
		return
	}
	if oe.Op != op || oe.Net != "tcp" {
		t.Errorf("OpError{Op: %q, Net: %q}, want {%q, \"tcp\"}", oe.Op, oe.Net, op)
	}
}

func checkTimeout(t *testing.T, err error, op string) {
	t.Helper()
	checkOpError(t, err, op, os.ErrDeadlineExceeded)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("err = %v, want a net.Error with Timeout() = true", err)
	}
}

// drain は、スタックが書いたデータのセグメントを、合わせて n バイトになるまで読む。
// SEQ が続いていることを確かめ、つなげたデータを返す。
func (p *peer) drain(t *testing.T, n int) []byte {
	t.Helper()
	var got []byte
	for len(got) < n {
		th, payload := p.dev.sentData(t)
		if th.Seq != p.ack {
			t.Fatalf("data segment Seq = %d, want %d", th.Seq, p.ack)
		}
		if len(payload) == 0 {
			t.Fatalf("segment with no data (flags %s) while draining", th.StringFlags())
		}
		p.ack += uint32(len(payload))
		got = append(got, payload...)
	}
	if len(got) != n {
		t.Fatalf("drained %d bytes, want %d", len(got), n)
	}
	return got
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// ---------------------------------------------------------------------------
// T8-4: Read / Write / Close / LocalAddr / RemoteAddr
// ---------------------------------------------------------------------------

func TestConn_Addrs(t *testing.T) {
	c, _ := newConn(t)

	for _, tt := range []struct {
		name string
		addr net.Addr
		want string
	}{
		{"LocalAddr", c.LocalAddr(), "10.10.0.2:80"},
		{"RemoteAddr", c.RemoteAddr(), "10.10.0.1:50001"},
	} {
		a, ok := tt.addr.(*net.TCPAddr)
		if !ok {
			t.Errorf("%s = %T, want *net.TCPAddr", tt.name, tt.addr)
			continue
		}
		if a.String() != tt.want {
			t.Errorf("%s = %s, want %s", tt.name, a, tt.want)
		}
	}
}

func TestConn_Read_BlocksUntilData(t *testing.T) {
	c, p := newConn(t)
	ch := readAsync(c, 100)
	blocked(t, ch)

	p.data(t, "hello")

	if r := wait(t, ch); r.err != nil || string(r.data) != "hello" {
		t.Errorf("Read = (%q, %v), want (\"hello\", nil)", r.data, r.err)
	}
}

func TestConn_Read_ShortBuffer_ReturnsRestNextTime(t *testing.T) {
	// 貯まっている分が p より多ければ、p の分だけ返し、残りは次の Read で返る
	c, p := newConn(t)
	p.data(t, "hello")

	if r := wait(t, readAsync(c, 2)); r.err != nil || string(r.data) != "he" {
		t.Errorf("first Read = (%q, %v), want (\"he\", nil)", r.data, r.err)
	}
	if r := wait(t, readAsync(c, 100)); r.err != nil || string(r.data) != "llo" {
		t.Errorf("second Read = (%q, %v), want (\"llo\", nil)", r.data, r.err)
	}
	blocked(t, readAsync(c, 100))
}

func TestConn_Read_DataBeforeAccept(t *testing.T) {
	// Accept の前に届いたデータは、Accept のあとで読める
	dev := newFakeDev()
	s := tuntcp.New(dev)
	defer s.Close()
	l := listen(t, s)
	iss := handshake(t, dev, 50001)
	p := &peer{dev: dev, port: 50001, seq: clientISN + 1, ack: iss + 1}
	p.data(t, "early")

	r := waitAccept(t, acceptAsync(l))
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}

	if got := wait(t, readAsync(r.conn, 100)); got.err != nil || string(got.data) != "early" {
		t.Errorf("Read = (%q, %v), want (\"early\", nil)", got.data, got.err)
	}
}

func TestConn_Read_EOFAfterPeerFIN(t *testing.T) {
	// 相手の FIN のあと、貯まっているデータを読み切ってから io.EOF。io.EOF は包まない (標準の net と同じ)
	c, p := newConn(t)
	p.data(t, "bye")
	p.fin(t)

	if r := wait(t, readAsync(c, 100)); r.err != nil || string(r.data) != "bye" {
		t.Errorf("first Read = (%q, %v), want (\"bye\", nil)", r.data, r.err)
	}
	for i := range 2 {
		if r := wait(t, readAsync(c, 100)); r.n != 0 || r.err != io.EOF {
			t.Errorf("Read %d after the data = (%d, %v), want (0, io.EOF)", i+1, r.n, r.err)
		}
	}
}

func TestConn_Read_BlockedReadGetsEOF(t *testing.T) {
	c, p := newConn(t)
	ch := readAsync(c, 100)
	blocked(t, ch)

	p.fin(t)

	if r := wait(t, ch); r.n != 0 || r.err != io.EOF {
		t.Errorf("Read = (%d, %v), want (0, io.EOF)", r.n, r.err)
	}
}

func TestConn_Read_ZeroLength(t *testing.T) {
	// 長さ 0 のバッファでは、待たずに (0, nil) を返す (標準の net と同じ)
	c, _ := newConn(t)

	if r := wait(t, readAsync(c, 0)); r.n != 0 || r.err != nil {
		t.Errorf("Read(empty) = (%d, %v), want (0, nil)", r.n, r.err)
	}
}

func TestConn_Write_SendsData(t *testing.T) {
	// Write は、送信バッファに積めた時点で返る (相手の ACK を待たない)。
	// データは MSS 536 ごとのセグメントになって出ていく
	c, p := newConn(t)
	data := pattern(1000)

	if r := wait(t, writeAsync(c, data)); r.n != 1000 || r.err != nil {
		t.Errorf("Write = (%d, %v), want (1000, nil)", r.n, r.err)
	}

	if got := p.drain(t, 1000); !bytes.Equal(got, data) {
		t.Error("the data on the wire differs from what was written")
	}
}

func TestConn_Write_BlocksWhileSendBufferFull(t *testing.T) {
	// 送信バッファ (65535 バイト) に入りきらない分は、ACK で空きができるまで待つ。
	// 全部積めたら返る
	c, p := newConn(t)
	data := pattern(70000)
	ch := writeAsync(c, data)

	got := p.drain(t, 65535) // 相手のウィンドウ 65535 の分は、すぐに出てくる
	blocked(t, ch)

	// 4464 バイトが ACK されても、残り 4465 バイトは入りきらない
	p.dev.inject(t, segmentFrom(p.port, tcp.FlagACK, p.seq, p.iss+1+4464, nil))
	p.dev.sync(t)
	blocked(t, ch)
	// もう 1 バイト ACK されると入る
	p.dev.inject(t, segmentFrom(p.port, tcp.FlagACK, p.seq, p.iss+1+4465, nil))

	if r := wait(t, ch); r.n != 70000 || r.err != nil {
		t.Errorf("Write = (%d, %v), want (70000, nil)", r.n, r.err)
	}
	got = append(got, p.drain(t, 4465)...)
	if !bytes.Equal(got, data) {
		t.Error("the data on the wire differs from what was written")
	}
}

func TestConn_Write_AfterPeerFIN(t *testing.T) {
	// 相手が送り終えても (CLOSE-WAIT)、こちらからは送れる
	c, p := newConn(t)
	p.fin(t)

	if r := wait(t, writeAsync(c, []byte("still here"))); r.n != 10 || r.err != nil {
		t.Errorf("Write = (%d, %v), want (10, nil)", r.n, r.err)
	}
	if got := p.drain(t, 10); string(got) != "still here" {
		t.Errorf("data on the wire = %q", got)
	}
}

func TestConn_Close_SendsFINAfterData(t *testing.T) {
	// Close は待たずに返る。送信バッファの残りを送ってから FIN を送る
	c, p := newConn(t)
	if r := wait(t, writeAsync(c, pattern(600))); r.err != nil {
		t.Fatalf("setup: Write: %v", r.err)
	}

	if err := c.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	p.drain(t, 600)
	checkSegment(t, p.dev.sent(t), tcp.FlagFIN|tcp.FlagACK, p.iss+1+600, clientISN+1)
}

func TestConn_Close_UnreadData_SendsRST(t *testing.T) {
	// RFC 9293 3.6.1 SHLD-3: 読み残しがあるまま閉じたら RST
	c, p := newConn(t)
	p.data(t, "unread")

	if err := c.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	checkSegment(t, p.dev.sent(t), tcp.FlagRST, p.iss+1, 0)
}

func TestConn_Close_DataArrivesAfterwards_SendsRST(t *testing.T) {
	// RFC 9293 3.6.1 SHLD-3: 閉じたあとに届いたデータには RST
	c, p := newConn(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkSegment(t, p.dev.sent(t), tcp.FlagFIN|tcp.FlagACK, p.iss+1, clientISN+1)

	p.send(t, tcp.FlagACK|tcp.FlagPSH, "late")

	checkSegment(t, p.dev.sent(t), tcp.FlagRST, p.iss+2, 0)
}

// ---------------------------------------------------------------------------
// T8-5: エラーの約束
// ---------------------------------------------------------------------------

func TestConn_Close_UnblocksReadAndWrite(t *testing.T) {
	// Close で中断された呼び出しは net.ErrClosed
	c, p := newConn(t)
	rd := readAsync(c, 100)
	wr := writeAsync(c, pattern(70000))
	p.drain(t, 65535)
	blocked(t, rd)
	blocked(t, wr)

	if err := c.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	r := wait(t, rd)
	checkOpError(t, r.err, "read", net.ErrClosed)
	w := wait(t, wr)
	checkOpError(t, w.err, "write", net.ErrClosed)
	if w.n != 65535 {
		t.Errorf("interrupted Write n = %d, want 65535 (what was buffered)", w.n)
	}
}

func TestConn_AfterClose_ReturnsErrClosed(t *testing.T) {
	// Close のあとの呼び出しは、どれも net.ErrClosed。2 回目の Close も
	c, _ := newConn(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	checkOpError(t, wait(t, readAsync(c, 10)).err, "read", net.ErrClosed)
	checkOpError(t, wait(t, writeAsync(c, []byte("x"))).err, "write", net.ErrClosed)
	checkOpError(t, c.Close(), "close", net.ErrClosed)
	checkOpError(t, c.SetDeadline(time.Now()), "set", net.ErrClosed)
	checkOpError(t, c.SetReadDeadline(time.Now()), "set", net.ErrClosed)
	checkOpError(t, c.SetWriteDeadline(time.Now()), "set", net.ErrClosed)
}

func TestConn_PeerRST_ReadAndWriteReturnECONNRESET(t *testing.T) {
	// 相手の RST で、待っている Read も、そのあとの Read と Write も ECONNRESET
	c, p := newConn(t)
	rd := readAsync(c, 100)
	blocked(t, rd)

	p.send(t, tcp.FlagRST, "")

	checkOpError(t, wait(t, rd).err, "read", syscall.ECONNRESET)
	checkOpError(t, wait(t, readAsync(c, 100)).err, "read", syscall.ECONNRESET)
	checkOpError(t, wait(t, writeAsync(c, []byte("x"))).err, "write", syscall.ECONNRESET)
	// Close は、消えた接続でも 1 回目は nil
	if err := c.Close(); err != nil {
		t.Errorf("Close after RST = %v, want nil", err)
	}
}

func TestConn_PeerRST_BufferedDataReadFirst(t *testing.T) {
	// RST の前に受け取ってあったデータは読める。そのあとで ECONNRESET
	c, p := newConn(t)
	p.data(t, "last words")
	p.send(t, tcp.FlagRST, "")

	if r := wait(t, readAsync(c, 100)); r.err != nil || string(r.data) != "last words" {
		t.Errorf("Read = (%q, %v), want the buffered data", r.data, r.err)
	}
	checkOpError(t, wait(t, readAsync(c, 100)).err, "read", syscall.ECONNRESET)
}

func TestConn_ListenerErrors_AreOpErrors(t *testing.T) {
	dev := newFakeDev()
	s := tuntcp.New(dev)
	defer s.Close()
	l := listen(t, s)

	_, err := s.Listen(localPort)
	checkOpError(t, err, "listen", syscall.EADDRINUSE)
	l.Close()
	_, err = l.Accept()
	checkOpError(t, err, "accept", net.ErrClosed)
	checkOpError(t, l.Close(), "close", net.ErrClosed)
}

func TestStack_Close_UnblocksConnCalls(t *testing.T) {
	// スタックを止めると、待っている Read は net.ErrClosed で返り、接続には RST が出る
	dev := newFakeDev()
	s := tuntcp.New(dev)
	c, p := newConnOn(t, dev, s)
	rd := readAsync(c, 100)
	blocked(t, rd)

	s.Close()

	checkOpError(t, wait(t, rd).err, "read", net.ErrClosed)
	checkSegment(t, p.dev.sent(t), tcp.FlagRST, p.iss+1, 0)
	checkOpError(t, wait(t, readAsync(c, 100)).err, "read", net.ErrClosed)
	checkOpError(t, wait(t, writeAsync(c, []byte("x"))).err, "write", net.ErrClosed)
	checkOpError(t, c.Close(), "close", net.ErrClosed)
}

// ---------------------------------------------------------------------------
// T8-6: deadline
// ---------------------------------------------------------------------------

func TestConn_SetReadDeadline_PastUnblocksRead(t *testing.T) {
	// net/http は、待っている Read をやめさせるのに、過去の時刻で SetReadDeadline を呼ぶ
	c, _ := newConn(t)
	ch := readAsync(c, 100)
	blocked(t, ch)

	if err := c.SetReadDeadline(time.Unix(1, 0)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	checkTimeout(t, wait(t, ch).err, "read")
	// 期限が過ぎたままなら、次の Read もすぐ返る
	checkTimeout(t, wait(t, readAsync(c, 100)).err, "read")
}

func TestConn_SetReadDeadline_FutureExpires(t *testing.T) {
	c, _ := newConn(t)
	start := time.Now()
	if err := c.SetReadDeadline(start.Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	ch := readAsync(c, 100)
	blocked(t, ch) // 50 ミリ秒の時点では、まだ待っている

	checkTimeout(t, wait(t, ch).err, "read")
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Errorf("Read returned after %v, want at least 100ms", d)
	}
}

func TestConn_SetReadDeadline_ZeroClears(t *testing.T) {
	// ゼロ値で期限がなくなる。過ぎていた期限も取り消せる
	c, p := newConn(t)
	c.SetReadDeadline(time.Unix(1, 0))
	checkTimeout(t, wait(t, readAsync(c, 100)).err, "read")

	if err := c.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("SetReadDeadline(zero): %v", err)
	}

	ch := readAsync(c, 100)
	blocked(t, ch)
	p.data(t, "ok")
	if r := wait(t, ch); r.err != nil || string(r.data) != "ok" {
		t.Errorf("Read = (%q, %v), want (\"ok\", nil)", r.data, r.err)
	}
}

func TestConn_SetReadDeadline_Extend(t *testing.T) {
	// 期限を先へ延ばすと、前の期限では返らない
	c, _ := newConn(t)
	c.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	ch := readAsync(c, 100)
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))

	blocked(t, ch) // 50 ミリ秒。前の期限 (30 ミリ秒) は過ぎている
	checkTimeout(t, wait(t, ch).err, "read")
}

func TestConn_ReadDeadline_ExpiredWithDataBuffered(t *testing.T) {
	// 期限が過ぎていれば、データが貯まっていてもタイムアウトを返す (標準の net と同じ)。データは失われない
	c, p := newConn(t)
	p.data(t, "kept")
	c.SetReadDeadline(time.Unix(1, 0))

	checkTimeout(t, wait(t, readAsync(c, 100)).err, "read")

	c.SetReadDeadline(time.Time{})
	if r := wait(t, readAsync(c, 100)); r.err != nil || string(r.data) != "kept" {
		t.Errorf("Read = (%q, %v), want (\"kept\", nil)", r.data, r.err)
	}
}

func TestConn_SetWriteDeadline_PastUnblocksWrite(t *testing.T) {
	// 待っている Write は、積めた分のバイト数とタイムアウトを返す
	c, p := newConn(t)
	ch := writeAsync(c, pattern(70000))
	p.drain(t, 65535)
	blocked(t, ch)

	if err := c.SetWriteDeadline(time.Unix(1, 0)); err != nil {
		t.Fatalf("SetWriteDeadline: %v", err)
	}

	r := wait(t, ch)
	checkTimeout(t, r.err, "write")
	if r.n != 65535 {
		t.Errorf("Write n = %d, want 65535", r.n)
	}
}

func TestConn_ReadAndWriteDeadlines_AreIndependent(t *testing.T) {
	c, p := newConn(t)

	c.SetReadDeadline(time.Unix(1, 0))
	if r := wait(t, writeAsync(c, []byte("w"))); r.n != 1 || r.err != nil {
		t.Errorf("Write with an expired read deadline = (%d, %v), want (1, nil)", r.n, r.err)
	}
	c.SetReadDeadline(time.Time{})

	c.SetWriteDeadline(time.Unix(1, 0))
	p.data(t, "r")
	if r := wait(t, readAsync(c, 10)); r.err != nil || string(r.data) != "r" {
		t.Errorf("Read with an expired write deadline = (%q, %v), want (\"r\", nil)", r.data, r.err)
	}
	checkTimeout(t, wait(t, writeAsync(c, []byte("w"))).err, "write")
}

func TestConn_SetDeadline_SetsBoth(t *testing.T) {
	c, _ := newConn(t)

	if err := c.SetDeadline(time.Unix(1, 0)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	checkTimeout(t, wait(t, readAsync(c, 10)).err, "read")
	checkTimeout(t, wait(t, writeAsync(c, []byte("x"))).err, "write")
}

// ---------------------------------------------------------------------------
// T8-7: 並行呼び出し。go test -race で確かめる
// ---------------------------------------------------------------------------

func TestConn_ConcurrentCalls(t *testing.T) {
	// Read、Write、deadline の設定、Close を、別々の goroutine から同時に呼ぶ。
	// 見るのは、競合がないこと (-race)、どの呼び出しも返ること、Close のあとは net.ErrClosed で終わること
	c, p := newConn(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	loop := func(f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				err := f()
				if errors.Is(err, net.ErrClosed) {
					return
				}
				select {
				case <-stop:
					if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
						t.Errorf("after stop: err = %v, want net.ErrClosed", err)
						return
					}
				default:
				}
			}
		}()
	}
	for range 2 {
		loop(func() error { _, err := c.Read(make([]byte, 16)); return err })
		loop(func() error { _, err := c.Write([]byte("0123456789")); return err })
	}
	loop(func() error { return c.SetReadDeadline(time.Now().Add(time.Millisecond)) })
	loop(func() error { return c.SetWriteDeadline(time.Now().Add(time.Millisecond)) })
	loop(func() error { return c.SetDeadline(time.Time{}) })

	// 相手はデータを送り続け、届いたデータに ACK を返す
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case pkt := <-p.dev.out:
				_ = pkt
			case p.dev.in <- segmentFrom(p.port, tcp.FlagACK|tcp.FlagPSH, p.seq+uint32(i), p.ack, []byte("x")):
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	close(stop)
	<-peerDone

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("some calls did not return within 2s after Close")
	}
}

func TestConn_ConcurrentWrites_DoNotInterleave(t *testing.T) {
	// 同時に呼んだ Write のデータは、呼び出しごとにまとまって出ていく (混ざらない)。
	// 1 回の Write が送信バッファより大きいときも
	c, p := newConn(t)
	a, b := bytes.Repeat([]byte("a"), 40000), bytes.Repeat([]byte("b"), 40000)
	cha := writeAsync(c, a)
	chb := writeAsync(c, b)

	var got []byte
	for len(got) < 80000 {
		_, payload := p.dev.sentData(t)
		got = append(got, payload...)
		// 受け取った分をすぐ ACK して、送信バッファを空ける
		p.dev.inject(t, segmentFrom(p.port, tcp.FlagACK, p.seq, p.iss+1+uint32(len(got)), nil))
	}
	if r := wait(t, cha); r.n != 40000 || r.err != nil {
		t.Errorf("Write a = (%d, %v)", r.n, r.err)
	}
	if r := wait(t, chb); r.n != 40000 || r.err != nil {
		t.Errorf("Write b = (%d, %v)", r.n, r.err)
	}

	if !bytes.Equal(got, append(append([]byte{}, a...), b...)) && !bytes.Equal(got, append(append([]byte{}, b...), a...)) {
		t.Error("the two writes are interleaved on the wire")
	}
}

// ---------------------------------------------------------------------------
// ループがタイマーを処理すること (T8-2 の残り)
// ---------------------------------------------------------------------------

// testClock は、テストが手で進める時計。ループの goroutine と同時に触るので、ロックで守る。
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestStack_TimeWaitExpires(t *testing.T) {
	// ループは一定の間隔でタイマーの期限を調べる。TIME-WAIT の接続は 2MSL (4 分) で消え、
	// 同じ相手のポートからの SYN で新しい接続を作れるようになる (RFC 9293 3.10.8)
	dev := newFakeDev()
	clk := &testClock{now: time.Unix(1_000_000, 0)}
	s := tuntcp.NewWithClock(dev, clk.Now, 5*time.Millisecond)
	defer s.Close()
	c, p := newConnOn(t, dev, s)

	// こちらが先に閉じ、相手が FIN|ACK を返す。TIME-WAIT に入る
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkSegment(t, dev.sent(t), tcp.FlagFIN|tcp.FlagACK, p.iss+1, clientISN+1)
	p.ack = p.iss + 2
	p.fin(t)

	// 4 分が経つ直前は、まだ TIME-WAIT。新しい SYN には SYN|ACK を返さない
	clk.advance(4*time.Minute - time.Nanosecond)
	time.Sleep(30 * time.Millisecond)
	dev.inject(t, segmentFrom(p.port, tcp.FlagSYN, 5000, 0, nil))
	dev.noSent(t)

	clk.advance(time.Nanosecond)
	time.Sleep(30 * time.Millisecond)
	dev.inject(t, segmentFrom(p.port, tcp.FlagSYN, 5000, 0, nil))
	th := dev.sent(t)
	checkSegment(t, th, tcp.FlagSYN|tcp.FlagACK, th.Seq, 5001)
}

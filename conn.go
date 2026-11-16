package tuntcp

import (
	"io"
	"net"
	"os"
	"syscall"
	"time"

	"go-tuntcp/internal/stack"
)

// Conn は、Listener.Accept が返す接続。net.Conn を満たす。
// メソッドは、どの goroutine から呼んでもよい。同時に呼んでもよい。
//
// どのメソッドも、ループの goroutine に関数を渡して実行させる (Stack.call)。
// Read と Write は、すぐに終えられなければ待ち行列に入り、ループが入力を 1 つ処理するたびに
// 進められるかを見直す (Stack.serveConns)。
type Conn struct {
	s             *Stack
	c             *stack.Connection
	local, remote *net.TCPAddr // 作ったあとは変えない

	// 以下は、ループの goroutine だけが触る

	closed bool // 利用者が Close を呼んだ

	// readers と writers は、待っている Read と Write。呼ばれた順に並び、先頭から順に済ませる。
	// 先頭を追い越さないので、同時に呼んだ Write のデータは混ざらない
	readers, writers []*ioOp

	rd, wd deadline
}

func newConn(s *Stack, c *stack.Connection) *Conn {
	// c.Key は、接続ができたあとは変わらないので、ループの外で読める
	return &Conn{
		s:      s,
		c:      c,
		local:  net.TCPAddrFromAddrPort(c.Key.Local),
		remote: net.TCPAddrFromAddrPort(c.Key.Remote),
	}
}

// ioOp は、待っている Read か Write 1 回分。
type ioOp struct {
	p    []byte        // 呼び出しが渡したバッファ。呼び出しは done を待っているので、ループが触ってよい
	n    int           // Write で、これまでに送信バッファに積めたバイト数
	done chan ioResult // 済んだら、ループが結果を送る。バッファを 1 つ持つので、送る側は待たない
}

type ioResult struct {
	n   int
	err error
}

// deadline は、Read か Write の期限。ループの goroutine だけが触る。
type deadline struct {
	t     time.Time   // ゼロ値なら期限なし
	timer *time.Timer // 期限が来たら、ループを起こす。期限なしか、設定した時点で過ぎていれば nil
	// fired は、timer が期限を知らせたこと。time.Now と t を比べるだけにしないのは、タイマーが鳴った時点の
	// 時計が t の手前に見えると (t に単調時計の読みがないときなど)、誰も起こさないまま待ち続けるため
	fired bool
	gen   int // set のたびに増やす。前の期限のタイマーが遅れて知らせてきたときに、見分ける
}

// set は、期限を t に変える。期限が来たら、ループに空の入力を送って起こす。
// ループは入力のたびに serveConns を呼ぶので、待っている呼び出しがそこで期限に気づく。
func (d *deadline) set(s *Stack, t time.Time) {
	d.stop()
	d.t = t
	if t.IsZero() {
		return
	}
	wait := time.Until(t)
	if wait <= 0 {
		return
	}
	gen := d.gen
	d.timer = time.AfterFunc(wait, func() {
		// スタックが止まっていれば、call は何もせずに返る
		_ = s.call(func() {
			if d.gen == gen {
				d.fired = true
			}
		})
	})
}

func (d *deadline) stop() {
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	d.t = time.Time{}
	d.fired = false
	d.gen++
}

func (d *deadline) expired() bool {
	return d.fired || (!d.t.IsZero() && !time.Now().Before(d.t))
}

// Read は、受け取ったデータを p に移す。データがなければ、届くまで待つ。
//
// 返すエラー (io.EOF のほかは *net.OpError で包む。docs/roadmap.md の T8-5、T8-6):
//   - Close のあとと、待っている間に Close されたとき: net.ErrClosed
//   - 読む期限が過ぎているとき: os.ErrDeadlineExceeded。データが貯まっていても返す (標準の net と同じ)
//   - 相手の FIN を受け、貯まっているデータを読み切ったあと: io.EOF
//   - 相手の RST を受け、貯まっているデータを読み切ったあと: syscall.ECONNRESET
//
// p の長さが 0 なら、待たずに (0, nil) を返す (標準の net と同じ)。
func (c *Conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return c.do("read", &c.readers, p)
}

// Write は、p を送信バッファに積む。全部積めたら返る。相手の ACK は待たない。
// 送信バッファ (65535 バイト) に空きがなければ、ACK で空くまで待つ。
//
// エラーのときは、それまでに積めたバイト数も返す。積めた分は送られる。
// 返すエラーは Read と同じ (io.EOF はない。期限は書く期限)。
func (c *Conn) Write(p []byte) (int, error) {
	return c.do("write", &c.writers, p)
}

// do は、Read と Write に共通の手順。待ち行列に入り、ループが済ませるのを待つ。
func (c *Conn) do(op string, queue *[]*ioOp, p []byte) (int, error) {
	o := &ioOp{p: p, done: make(chan ioResult, 1)}
	err := c.s.call(func() {
		*queue = append(*queue, o)
		// すぐに済ませられるなら、この関数のすぐあとの serveConns が済ませる
		c.s.waiting[c] = struct{}{}
	})
	if err != nil {
		return 0, c.opError(op, err)
	}
	// 待ち行列に入った呼び出しは、ループが必ず済ませる。ループが終わるときも、stopping を立てて済ませてから終わる
	r := <-o.done
	if r.err != nil && r.err != io.EOF {
		r.err = c.opError(op, r.err)
	}
	return r.n, r.err
}

// opError は、エラーを標準の net と同じ形に包む。ECONNRESET は、標準と同じく *os.SyscallError を挟む。
func (c *Conn) opError(op string, err error) error {
	if err == syscall.ECONNRESET {
		err = os.NewSyscallError(op, err)
	}
	return &net.OpError{Op: op, Net: "tcp", Source: c.local, Addr: c.remote, Err: err}
}

// serveConns は、待っている Read と Write を、進められるだけ進める。ループが入力を 1 つ処理するたびに呼ぶ。
// 待っている呼び出しが進められるようになるのは、受信 (データ、FIN、RST、送信バッファを空ける ACK)、
// API の呼び出し (Close、期限の変更)、期限のタイマーのどれかのあとで、どれもループの入力に当たる。
func (s *Stack) serveConns() {
	for c := range s.waiting {
		c.serve()
		if len(c.readers) == 0 && len(c.writers) == 0 {
			delete(s.waiting, c)
		}
	}
}

func (c *Conn) serve() {
	for len(c.readers) > 0 {
		o := c.readers[0]
		n, err, done := c.tryRead(o.p)
		if !done {
			break
		}
		o.done <- ioResult{n, err}
		c.readers = c.readers[1:]
	}
	for len(c.writers) > 0 {
		o := c.writers[0]
		err, done := c.tryWrite(o)
		if !done {
			break
		}
		o.done <- ioResult{o.n, err}
		c.writers = c.writers[1:]
	}
}

// tryRead は、Read を 1 回試す。済んだら done が true。まだ待つなら false。
func (c *Conn) tryRead(p []byte) (n int, err error, done bool) {
	switch {
	case c.closed || c.s.stopping:
		return 0, net.ErrClosed, true
	case c.rd.expired():
		return 0, os.ErrDeadlineExceeded, true
	}
	if n := c.c.Read(p); n > 0 {
		return n, nil, true
	}
	// 貯まっているデータを読み切ったあと。これ以上は届かないなら、その理由を返す
	switch {
	case c.c.Reset():
		return 0, syscall.ECONNRESET, true
	case c.c.PeerClosed():
		return 0, io.EOF, true
	}
	return 0, nil, false
}

// tryWrite は、Write の残りを、送信バッファに積めるだけ積む。全部積めたか、エラーなら done が true。
func (c *Conn) tryWrite(o *ioOp) (err error, done bool) {
	switch {
	case c.closed || c.s.stopping:
		return net.ErrClosed, true
	case c.wd.expired():
		return os.ErrDeadlineExceeded, true
	case c.c.Reset():
		return syscall.ECONNRESET, true
	}
	n, err := c.c.Write(o.p[o.n:])
	if err != nil {
		// 利用者が閉じておらず、相手の RST でもないのに、書けない状態になっている。
		// いまは起きない。再送の上限で接続を捨てるようになると起きる (T9-4)
		return err, true
	}
	o.n += n
	return nil, o.n == len(o.p)
}

// Close は、接続を手放す。待たずに返る。読む向きも書く向きも閉じる。
// 送信バッファに残っているデータは、送り切ってから FIN を送る。
// 待っている Read と Write は net.ErrClosed で返る。
//
// 受け取ったデータを読み残していれば、FIN ではなく RST を送って捨てる。閉じたあとにデータが届いたときも
// RST を送る (RFC 9293 3.6.1 SHLD-3。stack.Connection.Release)。
//
// 2 回目からと、Stack を止めたあとは、net.ErrClosed を返す。
func (c *Conn) Close() error {
	already := false
	err := c.s.call(func() {
		if c.closed {
			already = true
			return
		}
		c.closed = true
		c.rd.stop()
		c.wd.stop()
		c.c.Release()
		// 待っている Read と Write は、このすぐあとの serveConns が net.ErrClosed で返す
	})
	if err != nil || already {
		return c.opError("close", net.ErrClosed)
	}
	return nil
}

// LocalAddr は、自分のアドレス (届いたセグメントの宛先) を *net.TCPAddr で返す。
func (c *Conn) LocalAddr() net.Addr { return c.local }

// RemoteAddr は、相手のアドレスを *net.TCPAddr で返す。
func (c *Conn) RemoteAddr() net.Addr { return c.remote }

// SetDeadline は、読む期限と書く期限を同じ時刻にする。
func (c *Conn) SetDeadline(t time.Time) error {
	return c.setDeadline(t, true, true)
}

// SetReadDeadline は、Read の期限を決める。期限が来ると、待っている Read も、そのあとの Read も
// os.ErrDeadlineExceeded で返る。過去の時刻なら、すぐに返る。ゼロ値で期限がなくなる。
// net/http は、待っている Read をやめさせるのに、過去の時刻でこれを呼ぶ。
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.setDeadline(t, true, false)
}

// SetWriteDeadline は、Write の期限を決める。SetReadDeadline と同じ。
func (c *Conn) SetWriteDeadline(t time.Time) error {
	return c.setDeadline(t, false, true)
}

func (c *Conn) setDeadline(t time.Time, read, write bool) error {
	closed := false
	err := c.s.call(func() {
		if c.closed {
			closed = true
			return
		}
		if read {
			c.rd.set(c.s, t)
		}
		if write {
			c.wd.set(c.s, t)
		}
		// 期限がすでに過ぎていれば、待っている呼び出しは、このすぐあとの serveConns が返す
	})
	if err != nil || closed {
		return c.opError("set", net.ErrClosed)
	}
	return nil
}

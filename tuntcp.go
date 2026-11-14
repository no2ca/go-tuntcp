// Package tuntcp は、TUN デバイスの上で動くユーザー空間の TCP を、net.Listener と net.Conn の形で使えるようにする。
//
// プロトコルの判定は internal/stack が持つ。このパッケージが持つのは、goroutine、チャネル、
// デバイスの読み書きだけ (docs/design.md の「対話で決めた仕様」の「API とパッケージの形」)。
package tuntcp

import (
	"bytes"
	"io"
	"net"
	"sync"
	"time"

	"go-tuntcp/internal/ipv4"
	"go-tuntcp/internal/stack"
	"go-tuntcp/internal/tcp"
)

// maxPacket は、デバイスから 1 回に読む大きさの上限。IPv4 の Total Length が 16 ビットなので、
// これより大きいパケットはない (RFC 791 3.1)。
const maxPacket = 65535

// timeoutInterval は、ループが internal/stack のタイマーの期限を調べる間隔。
// いまあるタイマーは TIME-WAIT の 2MSL (4 分) だけなので、1 秒で足りる。
// 再送タイマー (M9) を入れるときに、次の期限まで待つ形に変える。
const timeoutInterval = time.Second

// Stack は、デバイス 1 つの上で動く TCP スタック。New で作り、Close で止める。
// メソッドは、どの goroutine から呼んでもよい。
type Stack struct {
	dev io.ReadWriteCloser

	// stk に触るのは、ループの goroutine (run) だけ。ロックは持たない。
	// ほかの goroutine は、call で関数をループに渡して実行させる
	stk *stack.Stack

	rx    chan []byte   // 読み取りの goroutine が、デバイスから読んだパケットを渡す
	calls chan func()   // API の呼び出しが、ループの上で実行する関数を渡す
	quit  chan struct{} // Close が閉じる。ループに終わるよう伝える
	done  chan struct{} // ループが終わったときに閉じる

	closeOnce sync.Once
	closeErr  error
}

// Listener は、Stack.Listen が返す待ち受け口。Accept / Close / Addr は T8-3 で足す。
type Listener struct{}

// New は、dev の上でスタックを動かし始める。goroutine を 2 つ起こす (デバイスを読むものと、ループ)。
// 止めるには Close を呼ぶ。
//
// dev は、1 回の Read で IP パケットを 1 つ返し、1 回の Write で 1 つ受け取るもの。
// Close を呼ぶと、待っている Read が返るものであること。返らないデバイスでは、
// 読み取りの goroutine が次のパケットが届くまで残る。
func New(dev io.ReadWriteCloser) *Stack {
	s := &Stack{
		dev:   dev,
		stk:   stack.New(),
		rx:    make(chan []byte),
		calls: make(chan func()),
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go s.readLoop()
	go s.run()
	return s
}

// readLoop は、デバイスからパケットを読んでループに渡す。デバイスの Read は待つので、ループとは別の goroutine にする。
// 読み取りがエラーを返したら終わる。Close でデバイスが閉じたときが、これに当たる。
//
// やっていないこと:
//   - Close 以外の理由で読めなくなったことを、利用者に伝える (決めていない。スタックは、
//     何も届かないまま動き続ける)
func (s *Stack) readLoop() {
	buf := make([]byte, maxPacket)
	for {
		n, err := s.dev.Read(buf)
		if err != nil {
			return
		}
		select {
		case s.rx <- bytes.Clone(buf[:n]):
		case <-s.done:
			return
		}
	}
}

// run は、イベントループ。受信したパケット、タイマー、API の呼び出しを 1 つずつ順に処理し、
// そのたびに、送信キューに積まれたパケットをデバイスに書く。
// internal/stack は「入力を 1 つ受けて TCB を更新し、送るセグメントを出す」状態機械なので、
// 入力を 1 本に並べれば、ロックなしで動かせる。
func (s *Stack) run() {
	defer close(s.done)
	tick := time.NewTicker(timeoutInterval)
	defer tick.Stop()
	for {
		select {
		case pkt := <-s.rx:
			s.receive(pkt)
		case <-tick.C:
			s.stk.HandleTimeouts()
		case fn := <-s.calls:
			fn()
		case <-s.quit:
			s.stk.AbortAll()
			s.flush()
			return
		}
		s.flush()
	}
}

// receive は、デバイスから読んだパケットを 1 つパースして internal/stack に渡す。
// IPv4 として読めないもの、TCP でないもの、TCP として読めないもの (チェックサムの不一致を含む) は、
// 何も返さずに捨てる。
func (s *Stack) receive(pkt []byte) {
	ip, ipPayload, err := ipv4.Parse(pkt)
	if err != nil || ip.Protocol != ipv4.ProtoTCP {
		return
	}
	t, _, payload, err := tcp.Parse(ip.Src.As4(), ip.Dst.As4(), ipPayload)
	if err != nil {
		return
	}
	s.stk.Handle(ip, t, payload)
}

// flush は、送信キューのパケットを、積まれた順にデバイスに書く。
//
// やっていないこと:
//   - 書き込みのエラーの扱い (決めていない。捨てる。相手から見れば、パケットが落ちたのと同じ)
func (s *Stack) flush() {
	for _, pkt := range s.stk.TakeOutgoing() {
		_, _ = s.dev.Write(pkt)
	}
}

// call は、fn をループの goroutine の上で実行し、終わるまで待つ。API のメソッドが stk に触るときに使う。
// スタックが止まっていれば、fn を実行せずに net.ErrClosed を返す。
func (s *Stack) call(fn func()) error {
	ran := make(chan struct{})
	select {
	case s.calls <- func() { fn(); close(ran) }:
		// calls にはバッファがないので、渡せたなら、ループが受け取って実行する
		<-ran
		return nil
	case <-s.done:
		return net.ErrClosed
	}
}

// Listen は、port で接続を待ち受ける。宛先の IP アドレスは見ない。
// 返ったあとに届いた SYN から、受け付ける。
// Close のあとは net.ErrClosed を返す (エラーを包む形は T8-5 で決める)。
func (s *Stack) Listen(port uint16) (*Listener, error) {
	if err := s.call(func() { s.stk.Listen(port) }); err != nil {
		return nil, err
	}
	return &Listener{}, nil
}

// Close は、スタックを止める。残っている接続に RST を送って捨て、ループを止め、デバイスを閉じる。
// 返ったときには、RST はデバイスに書かれている。2 回目からは何もせず、1 回目と同じ結果を返す。
func (s *Stack) Close() error {
	s.closeOnce.Do(func() {
		close(s.quit)
		<-s.done
		s.closeErr = s.dev.Close()
	})
	return s.closeErr
}

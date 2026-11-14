// Package tuntcp は、TUN デバイスの上で動くユーザー空間の TCP を、net.Listener と net.Conn の形で使えるようにする。
//
// プロトコルの判定は internal/stack が持つ。このパッケージが持つのは、goroutine、チャネル、
// デバイスの読み書きだけ (docs/design.md の「対話で決めた仕様」の「API とパッケージの形」)。
package tuntcp

import "io"

// Stack は、デバイス 1 つの上で動く TCP スタック。New で作り、Close で止める。
type Stack struct{}

// Listener は、Stack.Listen が返す待ち受け口。Accept / Close / Addr は T8-3 で足す。
type Listener struct{}

// New は、dev の上でスタックを動かし始める。
func New(dev io.ReadWriteCloser) *Stack {
	return &Stack{}
}

// Listen は、port で接続を待ち受ける。
func (s *Stack) Listen(port uint16) (*Listener, error) {
	return &Listener{}, nil
}

// Close は、スタックを止める。
func (s *Stack) Close() error {
	return nil
}

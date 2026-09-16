package stack

import (
	"crypto/rand"
	"encoding/binary"
	"go-tuntcp/internal/ipv4"
	"go-tuntcp/internal/tcp"
	"net/netip"
)

// RFC 9293 3.10.7.1 (CLOSED STATE)
func buildRST(ip ipv4.Header, t tcp.Header, payloadLen int) []byte {
	if t.HasFlag(tcp.FlagRST) {
		return nil
	}

	segLen := uint32(payloadLen)
	if t.HasFlag(tcp.FlagSYN) {
		segLen++
	}
	if t.HasFlag(tcp.FlagFIN) {
		segLen++
	}

	var seq, ack uint32
	var flags tcp.Flag
	switch {
	case t.HasFlag(tcp.FlagACK):
		seq = t.Ack
		ack = 0
		flags = tcp.FlagRST
	default:
		seq = 0
		ack = t.Seq + segLen
		flags = tcp.FlagRST | tcp.FlagACK
	}

	var wnd uint16 = 65535
	local := netip.AddrPortFrom(ip.Dst, t.DstPort)
	remote := netip.AddrPortFrom(ip.Src, t.SrcPort)

	seg := buildSegment(local, remote, flags, seq, ack, wnd, nil)

	return seg
}

func buildSegment(local, remote netip.AddrPort, flags tcp.Flag, seq, ack uint32, wnd uint16, payload []byte) []byte {
	tcpHdr := tcp.Header{
		SrcPort:    local.Port(),
		DstPort:    remote.Port(),
		Seq:        seq,
		Ack:        ack,
		DataOffset: 5,
		Flags:      flags,
		Window:     wnd,
	}

	ipPayload := tcpHdr.Serialize(local.Addr().As4(), remote.Addr().As4(), payload)

	ipHdr := ipv4.Header{
		Version:  4,
		IHL:      5,
		TotalLen: 40,
		TTL:      64,
		Protocol: ipv4.ProtoTCP,
		Src:      local.Addr(),
		Dst:      remote.Addr(),
	}

	seg := ipHdr.Serialize(ipPayload)

	return seg
}

type connKey struct {
	Local  netip.AddrPort
	Remote netip.AddrPort
}

type Connection struct {
	Key connKey
	tcp.ControlBlock
}

type Stack struct {
	listeners map[uint16]struct{}     // LISTEN中のポート
	conns     map[connKey]*Connection // 確立中の接続（SYN-RECEIVED以降）
	ISS       func() uint32           // 初期シーケンス番号の生成
}

func New() *Stack {
	return &Stack{
		listeners: nil,
		conns:     nil,
		ISS:       randomISS,
	}
}

func randomISS() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

func (s *Stack) Listen(port uint16) {
	s.listeners[port] = struct{}{}
}

func (s *Stack) Handle(ip ipv4.Header, t tcp.Header, payload []byte) []byte {
	key := connKey{
		Local:  netip.AddrPortFrom(ip.Dst, t.DstPort),
		Remote: netip.AddrPortFrom(ip.Src, t.SrcPort),
	}

	// 既存の接続
	if _, ok := s.conns[key]; ok {
		panic("stack.Stack.Handle: unreachable")
	}

	// 新しい接続
	if _, ok := s.listeners[t.DstPort]; ok {
		// ポートをlistenしているとき
		panic("stack.Stack.Handle: unreachable")
	}

	// ポートが閉じている場合
	return buildRST(ip, t, len(payload))
}

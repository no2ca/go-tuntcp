package stack

import (
	"crypto/rand"
	"encoding/binary"
	"go-tuntcp/internal/ipv4"
	"go-tuntcp/internal/tcp"
	"net/netip"
)

const (
	defaultWndSize = 65535
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

	var wnd uint16 = defaultWndSize
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

func (c *Connection) Handle(t tcp.Header, payload []byte) []byte {
	// 1: Sequence Number

	// 2: RST

	// 3: Security Check

	// 4: SYN
	if t.HasFlag(tcp.FlagSYN) {
		switch c.State {
		case tcp.StateSynReceived:
			c.State = tcp.StateListen
			return nil
		}
	}

	// 5: ACK
	if !t.HasFlag(tcp.FlagACK) {
		return nil
	}
	switch c.State {
	case tcp.StateSynReceived:
		c.State = tcp.StateEstablished
		c.Snd.UNA = t.Ack
		c.Snd.WND = t.Window
		c.Snd.WL1 = t.Seq
		c.Snd.WL2 = t.Ack
		// TODO: ACKが許容範囲外の場合RST
	}

	// 6: URG

	// 7: Segment Text

	return nil
}

type Stack struct {
	listeners map[uint16]struct{}     // LISTEN中のポート
	conns     map[connKey]*Connection // 確立中の接続（SYN-RECEIVED以降）
	ISS       func() uint32           // 初期シーケンス番号の生成
}

func New() *Stack {
	return &Stack{
		listeners: make(map[uint16]struct{}),
		conns:     make(map[connKey]*Connection),
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
	if c, ok := s.conns[key]; ok {
		return c.Handle(t, payload)
	}

	// RSTを受信したとき
	if t.HasFlag(tcp.FlagRST) {
		return nil
	}

	// 新しい接続 (RFC9293 3.10.7.2)
	if _, ok := s.listeners[t.DstPort]; ok {
		// TODO: セキュリティ情報が一致しない場合の処理について確認
		switch {
		case t.HasFlag(tcp.FlagACK):
			return buildRST(ip, t, len(payload))
		case t.HasFlag(tcp.FlagSYN):
			c := &Connection{Key: key}

			c.Rcv.NXT = t.Seq + 1
			c.Rcv.IRS = t.Seq
			c.Rcv.WND = defaultWndSize

			c.Snd.ISS = s.ISS()
			c.Snd.UNA = c.Snd.ISS
			c.Snd.NXT = c.Snd.ISS + 1

			c.State = tcp.StateSynReceived
			s.conns[key] = c

			return buildSegment(key.Local, key.Remote, tcp.FlagSYN|tcp.FlagACK, c.Snd.ISS, c.Rcv.NXT, c.Rcv.WND, nil)
		default:
			return nil
		}
	}

	// ポートが閉じているとき
	return buildRST(ip, t, len(payload))
}

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

	segLen := segmentLength(t, payloadLen)

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

// segmentLength は SEG.LEN を返す。SYN と FIN は、データを運ばなくても
// シーケンス番号を 1 つずつ使う (RFC 9293 3.4)。
func segmentLength(t tcp.Header, payloadLen int) uint32 {
	n := uint32(payloadLen)
	if t.HasFlag(tcp.FlagSYN) {
		n++
	}
	if t.HasFlag(tcp.FlagFIN) {
		n++
	}
	return n
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

	// RFC 791 3.1: Total Length は IP ヘッダ (オプションなしで 20) とデータを合わせた長さ
	ipHdr := ipv4.Header{
		Version:  4,
		IHL:      5,
		TotalLen: uint16(20 + len(ipPayload)),
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
	// RFC 9293 3.10.7.4 First: シーケンス番号の検査。
	// 受理できないセグメントは、相手にいまの RCV.NXT を教え直す ACK を返して捨てる。
	// 相手が古い状態のまま再送を続けているとき、この ACK で同期し直せる。
	// RST には返信しない。返すと、RST 同士の応酬になりうる。
	//
	// やっていないこと:
	//   - RCV.WND が 0 のときも、正しい ACK、URG、RST は受け付ける (未実装。M10 でゼロウィンドウを扱うときに決める)
	//   - 受理したセグメントのうち、ウィンドウの外にはみ出た部分を切り落とす
	//     (意図して省いた。RCV.NXT から始まらないデータは Seventh でまるごと捨てる)
	if !tcp.SegmentAcceptable(t.Seq, segmentLength(t, len(payload)), c.Rcv.NXT, c.Rcv.WND) {
		if t.HasFlag(tcp.FlagRST) {
			return nil
		}
		return buildSegment(c.Key.Local, c.Key.Remote, tcp.FlagACK, c.Snd.NXT, c.Rcv.NXT, c.Rcv.WND, nil)
	}

	// RFC 9293 3.10.7.4 Second: RST。
	// 相手がこの接続を捨てたという通知なので、こちらも捨てる。返信はしない。
	// 状態を LISTEN か CLOSED にしておけば、Stack.Handle が接続をテーブルから消す。
	// ここに来る RST は First を通っているので、SEG.SEQ は受信ウィンドウの中にある。
	//
	// やっていないこと:
	//   - RFC 5961 の challenge ACK (SEG.SEQ が RCV.NXT と一致しない RST には ACK を返すだけにする)。
	//     意図して省いた。目標のあとで足す。いまはウィンドウ内の RST ならどれでも接続を閉じる
	//   - 利用者への "connection reset" の通知と、再送キューの破棄 (未実装。通知する相手もキューもまだ無い。M8、M9)
	if t.HasFlag(tcp.FlagRST) {
		switch c.State {
		case tcp.StateSynReceived:
			// passive open しか無いので、常に LISTEN に戻る
			c.State = tcp.StateListen
		case tcp.StateEstablished:
			c.State = tcp.StateClosed
		}
		return nil
	}

	// RFC 9293 3.10.7.4 Third: セキュリティの検査は意図して省いた。

	// RFC 9293 3.10.7.4 Fourth: SYN。
	// やっていないこと: ESTABLISHED で受けた SYN の扱い (RFC 5961 の challenge ACK)。
	// 意図して省いた。目標のあとで足す。いまは SYN ビットを無視して先へ進む
	if t.HasFlag(tcp.FlagSYN) {
		switch c.State {
		case tcp.StateSynReceived:
			c.State = tcp.StateListen
			return nil
		}
	}

	// RFC 9293 3.10.7.4 Fifth: ACK。
	if !t.HasFlag(tcp.FlagACK) {
		return nil
	}
	switch c.State {
	case tcp.StateSynReceived:
		// こちらの SYN|ACK への ACK かを確かめる。以前の接続の残りの ACK で
		// ESTABLISHED になるのを防ぐ。範囲の外なら <SEQ=SEG.ACK><CTL=RST> を返し、
		// 状態は変えない (正しい ACK があとから届けば確立できる)。
		// SYN-RECEIVED では SND.UNA = ISS、SND.NXT = ISS+1 なので、通るのは SEG.ACK = ISS+1 だけ
		if !(tcp.SeqLT(c.Snd.UNA, t.Ack) && tcp.SeqLE(t.Ack, c.Snd.NXT)) {
			return buildSegment(c.Key.Local, c.Key.Remote, tcp.FlagRST, t.Ack, 0, c.Rcv.WND, nil)
		}
		c.State = tcp.StateEstablished
		c.Snd.UNA = t.Ack
		c.Snd.WND = t.Window
		c.Snd.WL1 = t.Seq
		c.Snd.WL2 = t.Ack
		// やっていないこと: ESTABLISHED での ACK の処理 (未実装。T6-6)。
		// 仕様では、ESTABLISHED に入ったあと続けてその処理を通る
	}

	// RFC 9293 3.10.7.4 Sixth: URG は未実装。扱いを決めていない。

	// RFC 9293 3.10.7.4 Seventh: データ。
	// 受け取った分だけ RCV.NXT を進め、<SEQ=SND.NXT><ACK=RCV.NXT><CTL=ACK> で相手に伝える。
	// データのない ACK には返信しない。返すと ACK 同士の応酬になる。
	//
	// RCV.NXT から始まらないデータ (先に届いた後ろのデータ、RCV.NXT をまたぐデータ) は、
	// いまの RCV.NXT を ACK で教えて、まるごと捨てる。相手の再送に任せる (docs/design.md の決めごと)。
	// 捨てた案: またぐデータの新しい部分だけを受け取る。再送は減るが、切り落としの境界の分岐が増える。
	//
	// やっていないこと:
	//   - データの保存 (未実装。T6-2)。いまは ACK だけ返してデータを捨てている
	//   - 先に届いた後ろのデータを取っておく (SHLD-31。意図して省いた)
	//   - またぐデータの新しい部分だけを処理する (First の冒頭の要求。意図して省いた)
	//   - ウィンドウの右端を越えた分の切り落とし (未実装。ウィンドウを 65535 で固定している間は、
	//     1 つの IP パケットに入るデータが越えることはない。M10 でウィンドウを動かすときに要る)
	//   - ACK を遅らせて送信データに相乗りさせる (未実装)
	if c.State == tcp.StateEstablished && len(payload) > 0 {
		if t.Seq == c.Rcv.NXT {
			c.Rcv.NXT += uint32(len(payload))
		}
		return buildSegment(c.Key.Local, c.Key.Remote, tcp.FlagACK, c.Snd.NXT, c.Rcv.NXT, c.Rcv.WND, nil)
	}

	// RFC 9293 3.10.7.4 Eighth: FIN は未実装 (T7-3)。

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
		reply := c.Handle(t, payload)
		// LISTEN か CLOSED に戻った接続はテーブルから消す。
		// LISTEN はポート単位で listeners が受け持つので、接続を残す必要はない
		if c.State == tcp.StateListen || c.State == tcp.StateClosed {
			delete(s.conns, key)
		}
		return reply
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

// Read は仮の読み出し口 (T6-2)。
func (c *Connection) Read(p []byte) int { return 0 }

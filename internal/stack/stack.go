package stack

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"go-tuntcp/internal/ipv4"
	"go-tuntcp/internal/tcp"
	"net/netip"
	"time"
)

const (
	defaultWndSize = 65535

	// defaultSendMSS は、MSS オプションを受け取っていないときの送信 MSS (RFC 9293 3.7.1 MUST-15)
	defaultSendMSS = 536
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

	// rcvBuf は、受け取ったが利用者がまだ読んでいないデータ。RCV.NXT の直前までが入っている。
	// 大きさは制限しておらず、RCV.WND とも結びついていない (M10 で上限を設け、空きから RCV.WND を決める)
	rcvBuf bytes.Buffer

	// sndBuf は、利用者が書いたが相手からまだ ACK を受けていないデータ。先頭が SND.UNA に当たる。
	// 先頭から SND.NXT - SND.UNA バイトは送信済み、残りは未送信。ACK を受けた分を先頭から捨てる。
	// 大きさは制限していない (M10)
	sndBuf bytes.Buffer

	// finSent は、自分の FIN を送ったかどうか。送ったあとは、FIN が SND.NXT-1 を使っている。
	// Close を呼んでも、送信バッファのデータを送り切るまで FIN は送らないので、状態だけでは分からない
	finSent bool

	// timeWait は TIME-WAIT の 2MSL タイマー。TIME-WAIT に入るときに動かす
	timeWait timer

	// stack は、この接続を持つ Stack。送るパケットを送信キューに積むときと、いまの時刻を得るときに使う
	stack *Stack
}

// timer は、期限を 1 つ持つタイマー。ゼロ値は止まっている。
// 自分では時間を測らない。動かすときと、期限が来たかを見るときに、Stack.Now で得た時刻を渡す。
// 期限が来たかは、Stack.HandleTimeouts が呼ばれたときに調べる。
type timer struct {
	deadline time.Time
	running  bool
}

func (t *timer) start(now time.Time, d time.Duration) {
	t.deadline = now.Add(d)
	t.running = true
}

func (t *timer) expired(now time.Time) bool {
	return t.running && !now.Before(t.deadline)
}

// msl は Maximum Segment Lifetime。セグメントがネットワークの中に残りうる時間の上限。
// RFC 9293 3.4.2: "For this specification the MSL is taken to be 2 minutes."
const msl = 2 * time.Minute

// enterTimeWait は、TIME-WAIT に入り、2MSL のタイマーを動かす
// (RFC 9293 3.10.7.4 の "enter the TIME-WAIT state, start the time-wait timer"、3.6.1 MUST-13)。
//
// やっていないこと:
//   - "turn off the other timers" (未実装。ほかのタイマーがまだない。再送タイマーは M9)
func (c *Connection) enterTimeWait() {
	c.State = tcp.StateTimeWait
	c.timeWait.start(c.stack.Now(), 2*msl)
}

// timeouts は、期限の来たタイマーを処理する。RFC 9293 3.10.8 (Timeouts)。Stack.HandleTimeouts が呼ぶ。
//
// やっていないこと:
//   - USER TIMEOUT (未実装。扱いを決めていない)
//   - RETRANSMISSION TIMEOUT (未実装。M9)
func (c *Connection) timeouts(now time.Time) {
	// TIME-WAIT TIMEOUT: "delete the TCB, enter the CLOSED state"。
	// CLOSED にしておけば、Stack.HandleTimeouts が接続をテーブルから消す
	if c.timeWait.expired(now) {
		c.State = tcp.StateClosed
	}
}

// Handle は、この接続に届いたセグメントを 1 つ処理する。Stack.Handle が呼ぶ。
// 送るものは、セグメントへの返信、データの順に送信キューに積む。
//
// 処理のあと、積んであったデータが送れるようになっていれば送る。
// 相手の ACK でウィンドウが開いたときや、ESTABLISHED になったときに当たる。
// 送るデータのセグメントにも ACK = RCV.NXT が載るので、セグメントの処理を終えてから送る。
func (c *Connection) Handle(t tcp.Header, payload []byte) {
	if reply := c.segmentArrives(t, payload); reply != nil {
		c.stack.send(reply)
	}
	c.output()
}

// segmentArrives は RFC 9293 3.10.7.4 (SEGMENT ARRIVES の、SYN-RECEIVED 以降の状態) を
// First から Eighth の順に写したもの。このセグメントへの返信を返す。返信がなければ nil。
// 返信を戻り値にしてあるのは、仕様の "send ... and return" をそのまま return 1 つで書くため。
// 送信キューに積むのは呼び出し元の Handle。
func (c *Connection) segmentArrives(t tcp.Header, payload []byte) []byte {
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
		// TIME-WAIT で相手の FIN の再送を受けたら、下の ACK を返すのに加えて、2MSL を数え直す。
		// 仕様は Fifth と Eighth の TIME-WAIT に "a retransmission of the remote FIN. Acknowledge it, and
		// restart the 2 MSL timeout" と書いているが、再送された FIN は SEG.SEQ が RCV.NXT の手前なので、
		// ここ (First) で受理できずに終わり、Fifth と Eighth には届かない。仕様の意図を、届く場所で実装する
		// (docs/design.md の「対話で決めた仕様」)。
		// 相手の FIN の再送と見なすのは、FIN の番号 (SEG.SEQ + データ長) が RCV.NXT-1 のものだけ
		if c.State == tcp.StateTimeWait && t.HasFlag(tcp.FlagFIN) && t.Seq+uint32(len(payload)) == c.Rcv.NXT-1 {
			c.timeWait.start(c.stack.Now(), 2*msl)
		}
		return buildSegment(c.Key.Local, c.Key.Remote, tcp.FlagACK, c.Snd.NXT, c.Rcv.NXT, c.Rcv.WND, nil)
	}

	// RFC 9293 3.10.7.4 Second: RST。
	// 相手がこの接続を捨てたという通知なので、こちらも捨てる。返信はしない。
	// SYN-RECEIVED は LISTEN に戻し、同期済みの状態は CLOSED にする。
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
		default:
			// 同期済みの状態 (ESTABLISHED から TIME-WAIT まで) は、どれも CLOSED にする
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
		c.Snd.WND = t.Window
		c.Snd.WL1 = t.Seq
		c.Snd.WL2 = t.Ack
		// 仕様では SND.UNA は下の ESTABLISHED の処理で進む。ここで先に進めておくのは、
		// SYN が使った 1 はデータではないので、送信バッファから取り除かせないため。
		// 下の処理からは重複した ACK に見え、SND.UNA と送信バッファには触らない
		c.Snd.UNA = t.Ack
		// 仕様どおり、続けて ESTABLISHED の処理を通る
		fallthrough
	case tcp.StateEstablished, tcp.StateCloseWait, tcp.StateFinWait1, tcp.StateFinWait2, tcp.StateClosing, tcp.StateLastAck:
		// ESTABLISHED の処理。CLOSE-WAIT でも同じことをする ("Do the same processing as for the ESTABLISHED state")。
		// 送ったデータへの ACK を受け、SND.UNA を進め、ACK されたデータを送信バッファから捨てる。
		// 重複した ACK (SEG.ACK =< SND.UNA) は無視するが、セグメントの処理は続ける
		// (ウィンドウの更新やデータの受け取りがありうる)。
		// まだ送っていない範囲への ACK は受け入れようがないので、いまの SND.NXT と RCV.NXT を ACK で伝え、
		// セグメントごと捨てる (載っているデータも受け取らない)
		//
		// やっていないこと:
		//   - 再送キューから、ACK されたセグメントを取り除く (未実装。再送キューがまだない。M9)
		//   - 利用者への "ok" の通知 (省略。Write は積んだ時点で返る)
		switch {
		case tcp.SeqLT(c.Snd.NXT, t.Ack):
			return buildSegment(c.Key.Local, c.Key.Remote, tcp.FlagACK, c.Snd.NXT, c.Rcv.NXT, c.Rcv.WND, nil)
		case tcp.SeqLT(c.Snd.UNA, t.Ack):
			// FIN まで ACK されたときは、捨てる長さが送信バッファの長さより 1 大きい (FIN は送信バッファにない)。
			// Next は、足りなければ残りをすべて捨てるだけなので、そのまま渡す
			c.sndBuf.Next(int(t.Ack - c.Snd.UNA))
			c.Snd.UNA = t.Ack
		}

		// 送信ウィンドウの更新。SND.WL1 と SND.WL2 は、最後にウィンドウを更新したセグメントの
		// SEG.SEQ と SEG.ACK。それより古いセグメントの SEG.WND で上書きしないための条件。
		// 上で SND.UNA を進めたあとなので、ここでは常に SEG.ACK =< SND.UNA。条件の SND.UNA =< SEG.ACK が
		// 成り立つのは等しいとき (いま SND.UNA が進んだか、新しい ACK を含まないセグメント) だけで、
		// 古い ACK (SEG.ACK < SND.UNA) は外れる
		if tcp.SeqLE(c.Snd.UNA, t.Ack) &&
			(tcp.SeqLT(c.Snd.WL1, t.Seq) || (c.Snd.WL1 == t.Seq && tcp.SeqLE(c.Snd.WL2, t.Ack))) {
			c.Snd.WND = t.Window
			c.Snd.WL1 = t.Seq
			c.Snd.WL2 = t.Ack
		}

		// 自分から閉じたあとの状態では、上の処理に加えて、自分の FIN が ACK されたかを見る。
		// FIN は最後に送るので、FIN を送り終えていて SND.UNA が SND.NXT に追いついていれば、FIN も ACK されている。
		// FIN を送る前 (送れないデータが残っている) は、SND.UNA = SND.NXT でも FIN への ACK ではない
		finAcked := c.finSent && c.Snd.UNA == c.Snd.NXT
		switch c.State {
		case tcp.StateFinWait1:
			// "if the FIN segment is now acknowledged, then enter FIN-WAIT-2 and continue processing in that state"
			if finAcked {
				c.State = tcp.StateFinWait2
			}
		case tcp.StateFinWait2:
			// "if the retransmission queue is empty, the user's CLOSE can be acknowledged" は、
			// 返す相手がいないので何もしない (Close は呼ばれた時点で返っている)
		case tcp.StateClosing:
			// "if the ACK acknowledges our FIN, then enter the TIME-WAIT state; otherwise, ignore the segment"
			if !finAcked {
				return nil
			}
			c.enterTimeWait()
		case tcp.StateLastAck:
			// "If our FIN is now acknowledged, delete the TCB, enter the CLOSED state, and return."
			// 相手の FIN には CLOSE-WAIT に入るときに ACK を返してあり、こちらが最後に送るのは FIN なので、
			// TIME-WAIT で待つ理由がない。Stack.Handle が接続をテーブルから消す
			if finAcked {
				c.State = tcp.StateClosed
				return nil
			}
		}
	case tcp.StateTimeWait:
		// 送ったものはすべて ACK されているので、ACK の処理はしない。ACK だけのセグメントに ACK を返すこともしない
		// (両側が TIME-WAIT のとき、応酬が止まらなくなる)。
		// 仕様がここに書いている「相手の FIN の再送に ACK を返し、2MSL を数え直す」は、First でやっている
	}

	// RFC 9293 3.10.7.4 Sixth: URG は未実装。扱いを決めていない。

	// 返信の ACK は、Seventh と Eighth を終えてから 1 つだけ作る。データと FIN が同じセグメントで
	// 届いたとき、両方をまとめて 1 つの ACK で返すため (MUST-58)
	ackNeeded := false

	// RFC 9293 3.10.7.4 Seventh: データ。
	// 受け取ったデータを受信バッファに移し、その分だけ RCV.NXT を進め、<SEQ=SND.NXT><ACK=RCV.NXT><CTL=ACK> で相手に伝える。
	// データのない ACK には返信しない。返すと ACK 同士の応酬になる。
	//
	// RCV.NXT から始まらないデータ (先に届いた後ろのデータ、RCV.NXT をまたぐデータ) は、
	// いまの RCV.NXT を ACK で教えて、まるごと捨てる。相手の再送に任せる (docs/design.md の決めごと)。
	// 捨てた案: またぐデータの新しい部分だけを受け取る。再送は減るが、切り落としの境界の分岐が増える。
	//
	// 相手の FIN を受けたあとの状態 (CLOSE-WAIT、CLOSING、LAST-ACK、TIME-WAIT) では、データは来ないはずなので
	// 無視する ("Ignore the segment text")。受け取らず、ACK も返さない。
	//
	// やっていないこと:
	//   - 先に届いた後ろのデータを取っておく (SHLD-31。意図して省いた)
	//   - またぐデータの新しい部分だけを処理する (First の冒頭の要求。意図して省いた)
	//   - ウィンドウの右端を越えた分の切り落とし (未実装。ウィンドウを 65535 で固定している間は、
	//     1 つの IP パケットに入るデータが越えることはない。M10 でウィンドウを動かすときに要る)
	//   - ACK を遅らせて送信データに相乗りさせる (未実装)
	receiving := c.State == tcp.StateEstablished || c.State == tcp.StateFinWait1 || c.State == tcp.StateFinWait2
	if receiving && len(payload) > 0 {
		if t.Seq == c.Rcv.NXT {
			c.rcvBuf.Write(payload)
			c.Rcv.NXT += uint32(len(payload))
		}
		ackNeeded = true
	}

	// RFC 9293 3.10.7.4 Eighth: FIN。
	// 相手が「もう送るものはない」と伝えてきた。FIN はシーケンス番号を 1 つ使う (3.4) ので、
	// RCV.NXT を 1 進めて ACK を返し、状態を移す。
	//
	// FIN を処理するのは、FIN の番号 (SEG.SEQ + データ長) が RCV.NXT と一致するときだけ。
	// 仕様は、ここに来るセグメントを「RCV.NXT から始まる理想のセグメント」と仮定している (First の末尾)。
	// 手前が抜けたまま FIN を受け取ると、抜けたデータを受け取らずに相手の送信が終わったことになってしまう。
	// 一致しない FIN (手前が抜けている、捨てたデータに付いていた) には、いまの RCV.NXT を ACK で返すだけにする (MAY-13)。
	// 受信済みのデータの再送に新しい FIN が付いているときは、データを Seventh で捨てたあと、ここで FIN だけを処理する。
	//
	// すでに FIN を受けた状態 (CLOSE-WAIT など) で RCV.NXT から始まる FIN がまた来たときも、
	// 仕様の文面どおり RCV.NXT を進めて ACK を返す (状態は "Remain")。相手の誤りでしか起きない。
	// 再送された FIN は SEG.SEQ が RCV.NXT の手前なので、First で落ちてここには来ない。
	//
	// やっていないこと:
	//   - 利用者への "connection closing" の通知 (未実装。Read が io.EOF を返すようにするのは T8-5)
	if t.HasFlag(tcp.FlagFIN) {
		if t.Seq+uint32(len(payload)) == c.Rcv.NXT {
			c.Rcv.NXT++
			switch c.State {
			case tcp.StateEstablished:
				// SYN-RECEIVED で受けた FIN もここに来る。Fifth を通った時点で ESTABLISHED になっている
				c.State = tcp.StateCloseWait
			case tcp.StateFinWait1:
				// 自分の FIN がまだ ACK されていない。両側が同時に閉じた (3.6 の Figure 13)。
				// 仕様の "If our FIN has been ACKed (perhaps in this segment), then enter TIME-WAIT" のほうは、
				// Fifth で FIN-WAIT-2 に移してあるので、下の FIN-WAIT-2 の分岐を通る
				c.State = tcp.StateClosing
			case tcp.StateFinWait2:
				c.enterTimeWait()
			case tcp.StateTimeWait:
				// "Remain in the TIME-WAIT state. Restart the 2 MSL time-wait timeout."
				c.timeWait.start(c.stack.Now(), 2*msl)
			}
			// CLOSE-WAIT、CLOSING、LAST-ACK は "Remain"
		}
		ackNeeded = true
	}

	if ackNeeded {
		return buildSegment(c.Key.Local, c.Key.Remote, tcp.FlagACK, c.Snd.NXT, c.Rcv.NXT, c.Rcv.WND, nil)
	}
	return nil
}

type Stack struct {
	listeners map[uint16]struct{}     // LISTEN中のポート
	conns     map[connKey]*Connection // 確立中の接続（SYN-RECEIVED以降）
	ISS       func() uint32           // 初期シーケンス番号の生成
	out       [][]byte                // 送信キュー。送るパケットはすべてここを通る。TakeOutgoing で取り出す
	Now       func() time.Time        // いまの時刻。タイマーの期限を決めるときと、期限が来たかを見るときに呼ぶ
}

func New() *Stack {
	return &Stack{
		listeners: make(map[uint16]struct{}),
		conns:     make(map[connKey]*Connection),
		ISS:       randomISS,
		Now:       time.Now,
	}
}

func randomISS() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

// Conn は、自分と相手のアドレスとポートの組で接続を引く。なければ nil。
//
// 仮の口。T6-7 のデモで、main が受信セグメントの宛先と送信元から接続を引き、
// Connection.Read と Write を呼ぶために使う。M8 で net.Listener の Accept に置き換える。
func (s *Stack) Conn(local, remote netip.AddrPort) *Connection {
	return s.conns[connKey{Local: local, Remote: remote}]
}

func (s *Stack) Listen(port uint16) {
	s.listeners[port] = struct{}{}
}

// Handle は、受信セグメントを 1 つ処理する。送るパケットは送信キューに積み、戻り値では返さない。
// 呼んだ側は、続けて TakeOutgoing で取り出して TUN に書く。
//
// 捨てた案: セグメントへの返信 (ACK、RST、SYN|ACK) だけを戻り値で返す (T7-0 までの形)。
// 送る順番を呼ぶ側が守ることになり、再送する SYN|ACK や Close で送る FIN のように、
// 受信を起点にしない送信と出口が分かれる。
func (s *Stack) Handle(ip ipv4.Header, t tcp.Header, payload []byte) {
	key := connKey{
		Local:  netip.AddrPortFrom(ip.Dst, t.DstPort),
		Remote: netip.AddrPortFrom(ip.Src, t.SrcPort),
	}

	// 既存の接続
	if c, ok := s.conns[key]; ok {
		c.Handle(t, payload)
		s.reap(c)
		return
	}

	if reply := s.segmentArrivesNoConn(key, ip, t, payload); reply != nil {
		s.send(reply)
	}
}

// reap は、LISTEN か CLOSED に戻った接続をテーブルから消す (仕様の "delete the TCB")。
// 接続の状態を変えうる処理 (受信セグメント、タイマー) のあとに呼ぶ。
// LISTEN はポート単位で listeners が受け持つので、接続を残す必要はない
func (s *Stack) reap(c *Connection) {
	if c.State == tcp.StateListen || c.State == tcp.StateClosed {
		delete(s.conns, c.Key)
	}
}

// HandleTimeouts は、期限の来たタイマーを処理する。RFC 9293 3.10.8 (Timeouts)。
// 呼ばれた時点の Now と期限を比べるので、呼ぶ間隔がタイマーの粒度になる。
// main が一定の間隔で呼び、続けて TakeOutgoing で取り出して TUN に書く
// (いまあるタイマーは何も送らないが、M9 の再送タイマーは送る)。
//
// 時刻を Handle や HandleTimeouts の引数で渡さず、Now で引くのは、Close や Write のような
// 利用者の呼び出しもタイマーを動かすようになる (M9 の再送) ため。呼び出しごとに時刻を渡させない。
func (s *Stack) HandleTimeouts() {
	now := s.Now()
	for _, c := range s.conns {
		c.timeouts(now)
		s.reap(c)
	}
}

func (s *Stack) send(pkt []byte) {
	s.out = append(s.out, pkt)
}

// segmentArrivesNoConn は、接続のないセグメントを処理し、返信を返す。返信がなければ nil。
// ポートが LISTEN 中なら RFC 9293 3.10.7.2、そうでなければ 3.10.7.1 に従う。
func (s *Stack) segmentArrivesNoConn(key connKey, ip ipv4.Header, t tcp.Header, payload []byte) []byte {
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
			c := &Connection{Key: key, stack: s}

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

// Read は、受信バッファに貯まったデータを p に移し、移したバイト数を返す。
// 貯まっていなければ待たずに 0 を返す。
//
// 仮の読み出し口。T6-7 のデモで main が呼ぶ。M8 で net.Conn の Read (データが来るまで
// ブロックする) に置き換える。
func (c *Connection) Read(p []byte) int {
	n, _ := c.rcvBuf.Read(p)
	return n
}

// Write は、p を送信バッファの後ろに積み、送れる分をすぐに送る。
// RFC 9293 3.10.2 SEND Call に従う。SYN-RECEIVED では積むだけにし、ESTABLISHED になってから送る。
// CLOSE-WAIT でも送れる (相手が送り終えただけで、こちらからの向きは開いている)。
// 自分から閉じたあとの状態と、接続がないときはエラーを返す。
//
// 仮の書き込み口。T6-7 のデモで main が呼ぶ。M8 で net.Conn の Write に置き換える。
// エラーの形 (net.OpError で包むかなど) は T8-5 で決める。
//
// やっていないこと:
//   - 送信バッファの上限と "insufficient resources" (未実装。M10)
func (c *Connection) Write(p []byte) error {
	switch c.State {
	case tcp.StateSynReceived, tcp.StateEstablished, tcp.StateCloseWait:
		c.sndBuf.Write(p)
		c.output()
		return nil
	case tcp.StateFinWait1, tcp.StateFinWait2, tcp.StateClosing, tcp.StateLastAck, tcp.StateTimeWait:
		// Close を呼んだあと。FIN より後ろにデータは送れない
		return errConnClosing
	default:
		return errConnNotExist
	}
}

// 利用者の呼び出しに返すエラー。文言は RFC 9293 3.10 の "error: ..." に合わせてある。
// エラーの形 (net.OpError で包むかなど) は T8-5 で決める。
var (
	errConnNotExist = errors.New("connection does not exist")
	errConnClosing  = errors.New("connection closing")
	// RFC にはないエラー。SYN-RECEIVED での Close を実装していないことを表す
	errCloseNotSupported = errors.New("close in SYN-RECEIVED is not supported")
)

// Close は、これ以上送るデータがないことを相手に伝える (FIN を送る)。RFC 9293 3.10.4 CLOSE Call に従う。
// 受け取る向きは閉じない。相手が FIN を送ってくるまで、Read で読み出せる (3.6)。
//
// FIN は、送信バッファのデータを送り切ったあとに送る ("Queue this until all preceding SENDs have been
// segmentized")。状態は、FIN を送る前でもここで移す。ESTABLISHED は FIN-WAIT-1 に、CLOSE-WAIT は LAST-ACK に。
// CLOSE-WAIT について、仕様の文面は「FIN を送ってから LAST-ACK に入る」と読めるが、ESTABLISHED の
// "In any case, enter FIN-WAIT-1 state" と同じ扱いにした (docs/design.md の「対話で決めた仕様」)。
// 捨てた案: FIN を送るまで CLOSE-WAIT に留め、Close 済みかどうかを別のフラグで持つ。
// Write を断るかどうかが、状態だけでは決まらなくなる。
//
// やっていないこと:
//   - SYN-RECEIVED での Close (意図して省いた。仕様は、送るデータがなければすぐ FIN を送って FIN-WAIT-1 に、
//     あれば ESTABLISHED まで待つとしている。M8 の Accept は ESTABLISHED の接続だけを返すので、
//     利用者が SYN-RECEIVED の接続を閉じる場面がない。エラーを返し、何も変えない)
//
// 仮の口。M8 で net.Conn の Close に置き換える。
func (c *Connection) Close() error {
	switch c.State {
	case tcp.StateEstablished:
		c.State = tcp.StateFinWait1
	case tcp.StateCloseWait:
		c.State = tcp.StateLastAck
	case tcp.StateSynReceived:
		return errCloseNotSupported
	case tcp.StateFinWait1, tcp.StateFinWait2, tcp.StateClosing, tcp.StateLastAck, tcp.StateTimeWait:
		// すでに閉じている。FIN-WAIT-1 と FIN-WAIT-2 について、仕様は "ok" を返すことも認めているが、
		// ほかの状態と揃えてエラーにする。どちらでも 2 つ目の FIN は送らない
		return errConnClosing
	default:
		return errConnNotExist
	}
	c.output()
	return nil
}

// output は、送信バッファのうちまだ送っていない部分を、送ってよい分だけセグメントに切り出して
// 送信キューに積み、SND.NXT を進める。Write のあとと、受信セグメントを処理したあとに呼ぶ。
// 受信のあとに呼ぶのは、相手の ACK でウィンドウが開いたり、ESTABLISHED になったりして、
// 積んであったデータが送れるようになるため。
//
// 送ってよいのは SND.NXT から SND.UNA + SND.WND の手前まで (RFC 9293 3.3.1 の図)。
// 1 セグメントは送信 MSS までにする。MSS オプションをまだ読まないので、
// RFC 9293 3.7.1 MUST-15 の既定値 536 を使う (M11 で相手の MSS に置き換える)。
// 送り切って未送信のデータがなくなるセグメントには PSH を立てる (RFC 9293 3.9.1.2 MUST-61。
// SEND の PUSH フラグは実装しないので、こちらに従う)。
//
// やっていないこと:
//   - 再送キュー (未実装。M9)。送ったデータは ACK されるまで送信バッファに残しておくだけ
//   - ウィンドウが 0 のときの probe (未実装。T10-2)
//   - Nagle と SWS 回避 (未実装。T10-3)。小さなセグメントもすぐに送る
func (c *Connection) output() {
	switch c.State {
	case tcp.StateEstablished, tcp.StateCloseWait:
	case tcp.StateFinWait1, tcp.StateClosing, tcp.StateLastAck:
		// Close のあと、自分の FIN がまだ ACK されていない状態。FIN を送ったら、もう送るものはない。
		// FIN のあとは SND.NXT - SND.UNA に FIN の 1 が含まれ、下の送信バッファの計算と合わなくなる
		if c.finSent {
			return
		}
	default:
		return
	}
	for {
		// sndBuf の先頭は SND.UNA に当たる。SND.NXT - SND.UNA までは送信済み
		sent := int(c.Snd.NXT - c.Snd.UNA)
		unsent := c.sndBuf.Len() - sent
		// 相手がウィンドウを縮めると、SND.NXT が右端を越えていることがある。そのときは負になる
		usable := int(int32(c.Snd.UNA + uint32(c.Snd.WND) - c.Snd.NXT))
		if n := min(unsent, usable, defaultSendMSS); n > 0 {
			flags := tcp.FlagACK
			if n == unsent {
				flags |= tcp.FlagPSH
			}
			data := c.sndBuf.Bytes()[sent : sent+n]
			c.stack.send(buildSegment(c.Key.Local, c.Key.Remote, flags, c.Snd.NXT, c.Rcv.NXT, c.Rcv.WND, data))
			c.Snd.NXT += uint32(n)
			continue
		}

		// 送れるデータがなくなった。Close 済みで、データをすべて送り終えていれば、FIN を送る
		// (RFC 9293 3.10.4)。<SEQ=SND.NXT><ACK=RCV.NXT><CTL=FIN,ACK> (3.6 の Figure 12)。
		// FIN もシーケンス番号を 1 つ使うので、相手のウィンドウに 1 つ分の空きがあるときだけ送る。
		// ウィンドウの外の FIN は、相手の First の検査 (Table 6) で受理されない
		// (docs/design.md の「対話で決めた仕様」)。
		// 捨てた案: 最後のデータのセグメントに FIN を載せる。セグメントは 1 つ減るが、
		// データと FIN の両方が収まるかどうかの分岐が増える
		closed := c.State == tcp.StateFinWait1 || c.State == tcp.StateClosing || c.State == tcp.StateLastAck
		if closed && unsent == 0 && usable >= 1 {
			c.stack.send(buildSegment(c.Key.Local, c.Key.Remote, tcp.FlagFIN|tcp.FlagACK, c.Snd.NXT, c.Rcv.NXT, c.Rcv.WND, nil))
			c.Snd.NXT++
			c.finSent = true
		}
		return
	}
}

// TakeOutgoing は、送信キューに積まれたパケットを積まれた順に返し、キューを空にする。
// main が、受信セグメントを Handle に渡したあとと、Connection.Write を呼んだあとに呼び、
// 返ったパケットを積まれた順に TUN に書く。
func (s *Stack) TakeOutgoing() [][]byte {
	out := s.out
	s.out = nil
	return out
}

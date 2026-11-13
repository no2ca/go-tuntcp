package stack

import (
	"errors"
	"testing"

	"go-tuntcp/internal/tcp"
)

// このファイルは M7 (切断) の振る舞いを検証する。
// RFC 9293 3.10.7.4 Eighth (FIN)、3.10.4 CLOSE Call、3.10.5 ABORT Call、3.10.8 Timeouts。

// mustState は、準備の途中で状態を確かめる。違っていれば、そこでテストを止める。
// 準備が崩れたまま先へ進むと、本題の検査の失敗と区別がつかなくなる。
func mustState(t *testing.T, c *Connection, want tcp.State) {
	t.Helper()
	if c.State != want {
		t.Fatalf("setup: State = %v, want %v", c.State, want)
	}
}

// toCloseWait は、ESTABLISHED で相手の FIN を受け、CLOSE-WAIT にした接続を返す。
// 相手の FIN は clientISN+1 を使うので、RCV.NXT は clientISN+2 になる。
func toCloseWait(t *testing.T, s *Stack) *Connection {
	t.Helper()
	c := establish(t, s)
	th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+1, testISS+1), nil))
	checkSegment(t, th, tcp.FlagACK, testISS+1, clientISN+2)
	mustState(t, c, tcp.StateCloseWait)
	return c
}

// toFinWait1 は、ESTABLISHED で Close を呼び、FIN-WAIT-1 にした接続を返す。
// 自分の FIN は testISS+1 を使うので、SND.NXT は testISS+2 になる。SND.UNA は testISS+1 のまま。
func toFinWait1(t *testing.T, s *Stack) *Connection {
	t.Helper()
	c := establish(t, s)
	if err := c.Close(); err != nil {
		t.Fatalf("setup: Close: %v", err)
	}
	segs := takeSent(t, s)
	if len(segs) != 1 {
		t.Fatalf("setup: Close sent %d segments, want 1 (FIN)", len(segs))
	}
	checkSegment(t, segs[0].hdr, tcp.FlagFIN|tcp.FlagACK, testISS+1, clientISN+1)
	mustState(t, c, tcp.StateFinWait1)
	return c
}

// toFinWait2 は、FIN-WAIT-1 で自分の FIN への ACK を受け、FIN-WAIT-2 にした接続を返す。
func toFinWait2(t *testing.T, s *Stack) *Connection {
	t.Helper()
	c := toFinWait1(t, s)
	if reply := handle(t, s, incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, testISS+2), nil); reply != nil {
		t.Fatalf("setup: reply to the ACK of our FIN = %x, want nil", reply)
	}
	mustState(t, c, tcp.StateFinWait2)
	return c
}

// toClosing は、FIN-WAIT-1 で相手の FIN を受け (自分の FIN への ACK はまだ)、CLOSING にした接続を返す。
// 両側が同時に閉じたときの状態 (RFC 9293 3.6 の Figure 13)。
func toClosing(t *testing.T, s *Stack) *Connection {
	t.Helper()
	c := toFinWait1(t, s)
	th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+1, testISS+1), nil))
	checkSegment(t, th, tcp.FlagACK, testISS+2, clientISN+2)
	mustState(t, c, tcp.StateClosing)
	return c
}

// toLastAck は、CLOSE-WAIT で Close を呼び、LAST-ACK にした接続を返す。
func toLastAck(t *testing.T, s *Stack) *Connection {
	t.Helper()
	c := toCloseWait(t, s)
	if err := c.Close(); err != nil {
		t.Fatalf("setup: Close: %v", err)
	}
	segs := takeSent(t, s)
	if len(segs) != 1 {
		t.Fatalf("setup: Close sent %d segments, want 1 (FIN)", len(segs))
	}
	checkSegment(t, segs[0].hdr, tcp.FlagFIN|tcp.FlagACK, testISS+1, clientISN+2)
	mustState(t, c, tcp.StateLastAck)
	return c
}

// toTimeWait は、FIN-WAIT-2 で相手の FIN を受け、TIME-WAIT にした接続を返す。
func toTimeWait(t *testing.T, s *Stack) *Connection {
	t.Helper()
	c := toFinWait2(t, s)
	th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+1, testISS+2), nil))
	checkSegment(t, th, tcp.FlagACK, testISS+2, clientISN+2)
	mustState(t, c, tcp.StateTimeWait)
	return c
}

// stateSetup は、状態ごとに同じ検査を繰り返す表で使う。
type stateSetup struct {
	name  string
	setup func(*testing.T, *Stack) *Connection
}

// afterPeerFIN は、相手の FIN を受けたあとの状態のうち、Close を経て入るもの。
// どれも RCV.NXT は clientISN+2。CLOSE-WAIT の分は T7-3 のテストにある。
var afterPeerFIN = []stateSetup{
	{"CLOSING", toClosing},
	{"LAST-ACK", toLastAck},
	{"TIME-WAIT", toTimeWait},
}

// ---------------------------------------------------------------------------
// T7-3: FIN の受信 (RFC 9293 3.10.7.4 Eighth)
// ---------------------------------------------------------------------------

func TestHandle_Established_FIN_EntersCloseWait(t *testing.T) {
	// Eighth: RCV.NXT を FIN の次に進め (FIN はシーケンス番号を 1 つ使う。3.4)、
	// <SEQ=SND.NXT><ACK=RCV.NXT><CTL=ACK> を返し、CLOSE-WAIT に入る
	s := newTestStack()
	c := establish(t, s)

	th, payload := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+1, testISS+1), nil))

	checkSegment(t, th, tcp.FlagACK, testISS+1, clientISN+2)
	if len(payload) != 0 {
		t.Errorf("payload = %q, want empty", payload)
	}
	if c.Rcv.NXT != clientISN+2 {
		t.Errorf("Rcv.NXT = %d, want %d (FIN consumes one sequence number)", c.Rcv.NXT, clientISN+2)
	}
	checkState(t, c, tcp.StateCloseWait)
	if s.conns[testKey()] != c {
		t.Error("connection removed from the table, want it kept until the user closes")
	}
}

func TestHandle_Established_DataAndFIN_OneACK(t *testing.T) {
	// データと FIN が 1 つのセグメントで届く。FIN の番号はデータの次 (SEG.SEQ + データ長)。
	// データを受信バッファに移し、データと FIN をまとめて 1 つの ACK で返す (Seventh、Eighth、MUST-58)
	s := newTestStack()
	c := establish(t, s)

	s.Handle(incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagPSH|tcp.FlagACK, clientISN+1, testISS+1), []byte("hello"))

	segs := takeSent(t, s)
	if len(segs) != 1 {
		t.Fatalf("sent %d segments, want 1 (one ACK for data and FIN)", len(segs))
	}
	checkSegment(t, segs[0].hdr, tcp.FlagACK, testISS+1, clientISN+1+5+1)
	if got := readAll(c); got != "hello" {
		t.Errorf("Read = %q, want %q", got, "hello")
	}
	checkState(t, c, tcp.StateCloseWait)
}

func TestHandle_Established_FINNotAtRcvNxt_NotProcessed(t *testing.T) {
	// FIN を処理するのは、FIN の番号 (SEG.SEQ + データ長) が RCV.NXT と一致するときだけ。
	// 手前のデータが抜けているのに FIN を受け取ると、抜けたデータを受け取らないまま
	// 「相手はもう送らない」と判断してしまう。
	// RCV.NXT から始まらないデータはまるごと捨てる決めごと (docs/design.md) なので、
	// そのデータに付いた FIN も処理しない。どの行も、いまの RCV.NXT を ACK で返すだけ
	const rcvNXT = clientISN + 1
	tests := []struct {
		name    string
		seq     uint32
		payload string
	}{
		{"FIN one ahead", rcvNXT + 1, ""},
		{"FIN one behind (First rejects it)", rcvNXT - 1, ""},
		{"data one ahead with FIN", rcvNXT + 1, "hello"},
		{"data one behind (overlapping) with FIN", rcvNXT - 1, "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := establish(t, s)

			th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, tt.seq, testISS+1), []byte(tt.payload)))

			checkSegment(t, th, tcp.FlagACK, testISS+1, rcvNXT)
			if c.Rcv.NXT != rcvNXT {
				t.Errorf("Rcv.NXT = %d, want %d (unchanged)", c.Rcv.NXT, rcvNXT)
			}
			checkState(t, c, tcp.StateEstablished)
			if got := readAll(c); got != "" {
				t.Errorf("Read = %q, want empty", got)
			}
		})
	}
}

func TestHandle_Established_DuplicateDataWithFIN_FINProcessed(t *testing.T) {
	// 受信済みのデータの再送に、まだ受け取っていない FIN が付いている。
	// データは RCV.NXT の手前なので捨てるが、FIN の番号 (SEG.SEQ + データ長) は RCV.NXT と一致する。
	// セグメントのうち新しい部分は FIN だけなので、FIN は処理する (First の冒頭: "only the new parts are processed")
	s := newTestStack()
	c := establish(t, s)
	s.Handle(incomingIP(), incomingTCP(tcp.FlagPSH|tcp.FlagACK, clientISN+1, testISS+1), []byte("hello"))
	takeSent(t, s)

	th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagPSH|tcp.FlagACK, clientISN+1, testISS+1), []byte("hello")))

	checkSegment(t, th, tcp.FlagACK, testISS+1, clientISN+1+5+1)
	checkState(t, c, tcp.StateCloseWait)
	if got := readAll(c); got != "hello" {
		t.Errorf("Read = %q, want %q (duplicate data must not be delivered twice)", got, "hello")
	}
}

func TestHandle_Established_FIN_SeqWrapsAround(t *testing.T) {
	// 相手の ISS = 0xfffffffe なら、FIN は 0xffffffff を使い、RCV.NXT は 2^32 を折り返して 0 になる
	const isn = uint32(0xfffffffe)
	s := newTestStack()
	s.Handle(incomingIP(), incomingTCP(tcp.FlagSYN, isn, 0), nil)
	s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, isn+1, testISS+1), nil)
	takeSent(t, s)
	c := s.conns[testKey()]
	if c == nil {
		t.Fatal("connection not registered")
	}
	mustState(t, c, tcp.StateEstablished)

	th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, 0xffffffff, testISS+1), nil))

	checkSegment(t, th, tcp.FlagACK, testISS+1, 0)
	if c.Rcv.NXT != 0 {
		t.Errorf("Rcv.NXT = %d, want 0", c.Rcv.NXT)
	}
	checkState(t, c, tcp.StateCloseWait)
}

func TestHandle_SynReceived_ACKAndFIN_EntersCloseWait(t *testing.T) {
	// handshake の最後の ACK に FIN が付いている。Fifth で ESTABLISHED になり、
	// 同じセグメントの Eighth で CLOSE-WAIT に入る
	s := newTestStack()
	c := sendSYN(t, s)

	th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+1, testISS+1), nil))

	checkSegment(t, th, tcp.FlagACK, testISS+1, clientISN+2)
	checkState(t, c, tcp.StateCloseWait)
}

// ---------------------------------------------------------------------------
// T7-3: CLOSE-WAIT (相手の FIN を受けたあと、利用者が閉じるまで)
// ---------------------------------------------------------------------------

func TestHandle_CloseWait_RetransmittedFIN_ReACKed(t *testing.T) {
	// こちらの ACK が届かなければ、相手は同じ FIN を再送する。再送された FIN は
	// RCV.NXT の 1 つ手前なので First で受理できず、いまの RCV.NXT を ACK で返す。状態は変えない
	s := newTestStack()
	c := toCloseWait(t, s)

	th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+1, testISS+1), nil))

	checkSegment(t, th, tcp.FlagACK, testISS+1, clientISN+2)
	if c.Rcv.NXT != clientISN+2 {
		t.Errorf("Rcv.NXT = %d, want %d (unchanged)", c.Rcv.NXT, clientISN+2)
	}
	checkState(t, c, tcp.StateCloseWait)
}

func TestHandle_CloseWait_DataIgnored(t *testing.T) {
	// Seventh、CLOSE-WAIT: 相手は FIN を送ったあとなので、データは来ないはず。来ても無視する。
	// 受信バッファに入れず、RCV.NXT も進めず、返信もしない
	s := newTestStack()
	c := toCloseWait(t, s)

	reply := handle(t, s, incomingIP(), incomingTCP(tcp.FlagPSH|tcp.FlagACK, clientISN+2, testISS+1), []byte("hello"))

	if reply != nil {
		t.Errorf("reply = %x, want nil", reply)
	}
	if c.Rcv.NXT != clientISN+2 {
		t.Errorf("Rcv.NXT = %d, want %d (unchanged)", c.Rcv.NXT, clientISN+2)
	}
	if got := readAll(c); got != "" {
		t.Errorf("Read = %q, want empty", got)
	}
	checkState(t, c, tcp.StateCloseWait)
}

func TestHandle_CloseWait_SecondFINAtRcvNxt_ACKed(t *testing.T) {
	// Eighth は、FIN が立っていれば状態を問わず RCV.NXT を進めて ACK を返すと書いている。
	// CLOSE-WAIT では "Remain in the CLOSE-WAIT state"。FIN を 2 回送るのは相手の誤りだが、
	// 仕様の文面どおりに扱う (再送された FIN は SEG.SEQ が RCV.NXT の手前なので、ここには来ない)
	s := newTestStack()
	c := toCloseWait(t, s)

	th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+2, testISS+1), nil))

	checkSegment(t, th, tcp.FlagACK, testISS+1, clientISN+3)
	checkState(t, c, tcp.StateCloseWait)
}

func TestWrite_CloseWait_SendsData(t *testing.T) {
	// 3.10.2 SEND Call、CLOSE-WAIT: 相手が送り終えても、こちらからはまだ送れる (half-close)。
	// ACK 番号は FIN の分まで進んだ RCV.NXT
	s := newTestStack()
	c := toCloseWait(t, s)

	if err := c.Write([]byte("hello")); err != nil {
		t.Fatalf("Write in CLOSE-WAIT: %v", err)
	}

	segs := takeSent(t, s)
	if len(segs) != 1 {
		t.Fatalf("sent %d segments, want 1", len(segs))
	}
	checkSegment(t, segs[0].hdr, tcp.FlagACK|tcp.FlagPSH, testISS+1, clientISN+2)
	if string(segs[0].payload) != "hello" {
		t.Errorf("payload = %q, want %q", segs[0].payload, "hello")
	}
}

func TestHandle_CloseWait_ACK_SendsRemainingData(t *testing.T) {
	// Fifth、CLOSE-WAIT: "Do the same processing as for the ESTABLISHED state"。
	// ACK で SND.UNA が進み、ウィンドウが開いて、残りのデータが送られる
	s := newTestStack()
	c := establishWithWindow(t, s, 5)
	fin := incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+1, testISS+1)
	fin.Window = 5 // FIN のセグメントもウィンドウを広告する。5 のままにしておく
	s.Handle(incomingIP(), fin, nil)
	takeSent(t, s) // FIN への ACK
	mustState(t, c, tcp.StateCloseWait)
	if err := c.Write([]byte("hello world")); err != nil {
		t.Fatalf("Write in CLOSE-WAIT: %v", err)
	}
	if segs := takeSent(t, s); len(segs) != 1 || string(segs[0].payload) != "hello" {
		t.Fatalf("first flight = %v, want one segment %q", segs, "hello")
	}

	ack := incomingTCP(tcp.FlagACK, clientISN+2, testISS+6)
	ack.Window = 100
	s.Handle(incomingIP(), ack, nil)

	if c.Snd.UNA != testISS+6 {
		t.Errorf("Snd.UNA = %d, want %d", c.Snd.UNA, testISS+6)
	}
	segs := takeSent(t, s)
	if len(segs) != 1 {
		t.Fatalf("sent %d segments after the ACK, want 1", len(segs))
	}
	checkSegment(t, segs[0].hdr, tcp.FlagACK|tcp.FlagPSH, testISS+6, clientISN+2)
	if string(segs[0].payload) != " world" {
		t.Errorf("payload = %q, want %q", segs[0].payload, " world")
	}
}

func TestHandle_CloseWait_RST_Closes(t *testing.T) {
	// Second、CLOSE-WAIT: RST を受けたら CLOSED にして TCB を消す。返信はしない
	s := newTestStack()
	c := toCloseWait(t, s)

	reply := handle(t, s, incomingIP(), incomingTCP(tcp.FlagRST, clientISN+2, 0), nil)

	if reply != nil {
		t.Errorf("reply = %x, want nil", reply)
	}
	checkState(t, c, tcp.StateClosed)
	if _, ok := s.conns[testKey()]; ok {
		t.Error("connection still in the table after RST")
	}
}

// ---------------------------------------------------------------------------
// T7-4: 自分から閉じる (RFC 9293 3.10.4 CLOSE Call)
// ---------------------------------------------------------------------------

func TestClose_Established_SendsFIN_EntersFinWait1(t *testing.T) {
	// 3.10.4、ESTABLISHED: 送るデータが残っていなければ、すぐ <SEQ=SND.NXT><ACK=RCV.NXT><CTL=FIN,ACK> を送る。
	// FIN はシーケンス番号を 1 つ使うので、SND.NXT が 1 進む
	s := newTestStack()
	c := establish(t, s)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	segs := takeSent(t, s)
	if len(segs) != 1 {
		t.Fatalf("sent %d segments, want 1 (FIN)", len(segs))
	}
	checkSegment(t, segs[0].hdr, tcp.FlagFIN|tcp.FlagACK, testISS+1, clientISN+1)
	if len(segs[0].payload) != 0 {
		t.Errorf("payload = %q, want empty", segs[0].payload)
	}
	if c.Snd.NXT != testISS+2 {
		t.Errorf("Snd.NXT = %d, want %d (FIN consumes one sequence number)", c.Snd.NXT, testISS+2)
	}
	checkState(t, c, tcp.StateFinWait1)
}

func TestClose_CloseWait_SendsFIN_EntersLastAck(t *testing.T) {
	// 3.10.4、CLOSE-WAIT: FIN を送って LAST-ACK に入る。ACK 番号は相手の FIN の次
	s := newTestStack()
	c := toCloseWait(t, s)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	segs := takeSent(t, s)
	if len(segs) != 1 {
		t.Fatalf("sent %d segments, want 1 (FIN)", len(segs))
	}
	checkSegment(t, segs[0].hdr, tcp.FlagFIN|tcp.FlagACK, testISS+1, clientISN+2)
	checkState(t, c, tcp.StateLastAck)
}

func TestClose_AfterWrite_FINFollowsData(t *testing.T) {
	// FIN の番号は、それまでに送ったデータの次
	s := newTestStack()
	c := establish(t, s)
	c.Write([]byte("hello"))
	takeSent(t, s)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	segs := takeSent(t, s)
	if len(segs) != 1 {
		t.Fatalf("sent %d segments, want 1 (FIN)", len(segs))
	}
	checkSegment(t, segs[0].hdr, tcp.FlagFIN|tcp.FlagACK, testISS+6, clientISN+1)
	if c.Snd.NXT != testISS+7 {
		t.Errorf("Snd.NXT = %d, want %d", c.Snd.NXT, testISS+7)
	}
}

func TestClose_UnsentData_FINWaitsUntilDataIsSent(t *testing.T) {
	// 3.10.4、ESTABLISHED: "Queue this until all preceding SENDs have been segmentized, then form a FIN
	// segment and send it. In any case, enter FIN-WAIT-1 state."
	// ウィンドウが足りずに送れないデータが残っている間、FIN は送らない。状態は先に FIN-WAIT-1 になる
	s := newTestStack()
	c := establishWithWindow(t, s, 5)
	c.Write([]byte("hello world"))
	takeSent(t, s) // "hello"

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if segs := takeSent(t, s); len(segs) != 0 {
		t.Fatalf("Close sent %d segments, want 0 (unsent data remains)", len(segs))
	}
	checkState(t, c, tcp.StateFinWait1)

	// "hello" への ACK でウィンドウが開く。残りのデータ、FIN の順に送る
	ack := incomingTCP(tcp.FlagACK, clientISN+1, testISS+6)
	ack.Window = 100
	s.Handle(incomingIP(), ack, nil)

	segs := takeSent(t, s)
	if len(segs) != 2 {
		t.Fatalf("sent %d segments after the ACK, want 2 (data, FIN)", len(segs))
	}
	checkSegment(t, segs[0].hdr, tcp.FlagACK|tcp.FlagPSH, testISS+6, clientISN+1)
	if string(segs[0].payload) != " world" {
		t.Errorf("payload = %q, want %q", segs[0].payload, " world")
	}
	checkSegment(t, segs[1].hdr, tcp.FlagFIN|tcp.FlagACK, testISS+12, clientISN+1)
	// この ACK は、その時点で送っていたデータをすべて ACK した (SND.UNA = SND.NXT だった)。
	// FIN はまだ送っていなかったので、FIN への ACK ではない
	checkState(t, c, tcp.StateFinWait1)

	// データだけへの ACK。FIN (testISS+12) はまだ ACK されていない
	s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, testISS+12), nil)
	checkState(t, c, tcp.StateFinWait1)
	// FIN への ACK
	s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, testISS+13), nil)
	checkState(t, c, tcp.StateFinWait2)
	if segs := takeSent(t, s); len(segs) != 0 {
		t.Errorf("sent %d segments after the ACKs, want 0", len(segs))
	}
}

func TestClose_FINNeedsSendWindow(t *testing.T) {
	// FIN もシーケンス番号を 1 つ使うので、相手のウィンドウに 1 つ分の空きが要る
	// (docs/design.md の「対話で決めた仕様」)。ウィンドウの外の FIN は、相手の First の検査で捨てられる
	t.Run("window exactly used up by data: FIN waits", func(t *testing.T) {
		s := newTestStack()
		c := establishWithWindow(t, s, 5)
		c.Write([]byte("hello"))
		takeSent(t, s)

		c.Close()
		if segs := takeSent(t, s); len(segs) != 0 {
			t.Fatalf("Close sent %d segments, want 0 (no room in the send window)", len(segs))
		}

		// データへの ACK が来ても、ウィンドウが 0 なら送れない
		ack := incomingTCP(tcp.FlagACK, clientISN+1, testISS+6)
		ack.Window = 0
		s.Handle(incomingIP(), ack, nil)
		if segs := takeSent(t, s); len(segs) != 0 {
			t.Fatalf("sent %d segments with a zero window, want 0", len(segs))
		}
		checkState(t, c, tcp.StateFinWait1)

		// ウィンドウが 1 開けば送れる
		ack = incomingTCP(tcp.FlagACK, clientISN+1, testISS+6)
		ack.Window = 1
		s.Handle(incomingIP(), ack, nil)
		segs := takeSent(t, s)
		if len(segs) != 1 {
			t.Fatalf("sent %d segments with window 1, want 1 (FIN)", len(segs))
		}
		checkSegment(t, segs[0].hdr, tcp.FlagFIN|tcp.FlagACK, testISS+6, clientISN+1)
	})
	t.Run("one more than the data: FIN goes out with the data", func(t *testing.T) {
		s := newTestStack()
		c := establishWithWindow(t, s, 6)

		c.Write([]byte("hello"))
		c.Close()

		segs := takeSent(t, s)
		if len(segs) != 2 {
			t.Fatalf("sent %d segments, want 2 (data, FIN)", len(segs))
		}
		checkSegment(t, segs[1].hdr, tcp.FlagFIN|tcp.FlagACK, testISS+6, clientISN+1)
	})
}

func TestClose_FIN_SndNxtWrapsAround(t *testing.T) {
	// 自分の ISS = 0xfffffffe なら、FIN は 0xffffffff を使い、SND.NXT は折り返して 0 になる。
	// FIN への ACK は SEG.ACK = 0
	s := New()
	s.ISS = func() uint32 { return 0xfffffffe }
	s.Listen(localPort)
	s.Handle(incomingIP(), incomingTCP(tcp.FlagSYN, clientISN, 0), nil)
	s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, 0xffffffff), nil)
	takeSent(t, s)
	c := s.conns[testKey()]
	if c == nil {
		t.Fatal("connection not registered")
	}
	mustState(t, c, tcp.StateEstablished)

	c.Close()

	segs := takeSent(t, s)
	if len(segs) != 1 {
		t.Fatalf("sent %d segments, want 1 (FIN)", len(segs))
	}
	checkSegment(t, segs[0].hdr, tcp.FlagFIN|tcp.FlagACK, 0xffffffff, clientISN+1)
	if c.Snd.NXT != 0 {
		t.Errorf("Snd.NXT = %d, want 0", c.Snd.NXT)
	}
	s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, 0), nil)
	checkState(t, c, tcp.StateFinWait2)
}

func TestClose_AlreadyClosing_ReturnsError(t *testing.T) {
	// 3.10.4: すでに Close を呼んだあとの状態では "error: connection closing"。2 つ目の FIN は送らない。
	// TCB がなければ "error: connection does not exist"。
	// SYN-RECEIVED での Close は実装していない (docs/design.md)。エラーを返し、何も変えない
	closed := func(t *testing.T, s *Stack) *Connection {
		c := establish(t, s)
		s.Handle(incomingIP(), incomingTCP(tcp.FlagRST, clientISN+1, 0), nil)
		mustState(t, c, tcp.StateClosed)
		return c
	}
	tests := []struct {
		stateSetup
		want error
	}{
		{stateSetup{"FIN-WAIT-1", toFinWait1}, errConnClosing},
		{stateSetup{"FIN-WAIT-2", toFinWait2}, errConnClosing},
		{stateSetup{"CLOSING", toClosing}, errConnClosing},
		{stateSetup{"LAST-ACK", toLastAck}, errConnClosing},
		{stateSetup{"TIME-WAIT", toTimeWait}, errConnClosing},
		{stateSetup{"CLOSED", closed}, errConnNotExist},
		{stateSetup{"SYN-RECEIVED", sendSYN}, errCloseNotSupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := tt.setup(t, s)
			before := c.ControlBlock

			if err := c.Close(); !errors.Is(err, tt.want) {
				t.Errorf("Close: err = %v, want %v", err, tt.want)
			}
			if segs := takeSent(t, s); len(segs) != 0 {
				t.Errorf("Close sent %d segments, want 0", len(segs))
			}
			if c.ControlBlock != before {
				t.Errorf("TCB changed: %+v, want %+v", c.ControlBlock, before)
			}
		})
	}
}

func TestWrite_AfterClose_ReturnsError(t *testing.T) {
	// 3.10.2 SEND Call: 自分から閉じたあとの状態では "error: connection closing"。何も送らない
	for _, tt := range []stateSetup{
		{"FIN-WAIT-1", toFinWait1},
		{"FIN-WAIT-2", toFinWait2},
		{"CLOSING", toClosing},
		{"LAST-ACK", toLastAck},
		{"TIME-WAIT", toTimeWait},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := tt.setup(t, s)

			if err := c.Write([]byte("hello")); !errors.Is(err, errConnClosing) {
				t.Errorf("Write: err = %v, want %v", err, errConnClosing)
			}
			if segs := takeSent(t, s); len(segs) != 0 {
				t.Errorf("Write sent %d segments, want 0", len(segs))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// T7-4: 自分の FIN への ACK (RFC 9293 3.10.7.4 Fifth)
// ---------------------------------------------------------------------------

func TestHandle_FinWait1_ACK(t *testing.T) {
	// Fifth、FIN-WAIT-1: ESTABLISHED の処理に加えて、自分の FIN が ACK されたら FIN-WAIT-2 に入る。
	// toFinWait1 のあとは SND.UNA = testISS+1、FIN は testISS+1、SND.NXT = testISS+2
	tests := []struct {
		name      string
		ack       uint32
		wantReply bool
		wantState tcp.State
	}{
		{"SEG.ACK = SND.UNA: FIN not acknowledged", testISS + 1, false, tcp.StateFinWait1},
		{"SEG.ACK = SND.NXT: FIN acknowledged", testISS + 2, false, tcp.StateFinWait2},
		{"SEG.ACK = SND.NXT+1: not yet sent, ACK and drop", testISS + 3, true, tcp.StateFinWait1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := toFinWait1(t, s)

			reply := handle(t, s, incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, tt.ack), nil)

			if tt.wantReply {
				th, _ := mustReply(t, reply)
				checkSegment(t, th, tcp.FlagACK, testISS+2, clientISN+1)
			} else if reply != nil {
				t.Errorf("reply = %x, want nil (FIN must not be sent twice)", reply)
			}
			checkState(t, c, tt.wantState)
		})
	}
}

func TestHandle_FinWait1_DataACKedButNotFIN_StaysFinWait1(t *testing.T) {
	// データ 5 バイトと FIN を送ったあと。データの末尾までの ACK (testISS+6) は FIN を含まない。
	// FIN の次 (testISS+7) まで ACK されて、初めて FIN-WAIT-2 に入る
	s := newTestStack()
	c := establish(t, s)
	c.Write([]byte("hello"))
	c.Close()
	takeSent(t, s)
	mustState(t, c, tcp.StateFinWait1)

	s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, testISS+6), nil)
	checkState(t, c, tcp.StateFinWait1)

	s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+1, testISS+7), nil)
	checkState(t, c, tcp.StateFinWait2)
	if c.Snd.UNA != testISS+7 {
		t.Errorf("Snd.UNA = %d, want %d", c.Snd.UNA, testISS+7)
	}
}

func TestHandle_Closing_ACK(t *testing.T) {
	// Fifth、CLOSING: 自分の FIN が ACK されたら TIME-WAIT に入る。そうでなければセグメントを無視する。
	// toClosing のあとは RCV.NXT = clientISN+2、SND.UNA = testISS+1、SND.NXT = testISS+2
	tests := []struct {
		name      string
		ack       uint32
		wantState tcp.State
	}{
		{"SEG.ACK = SND.UNA: FIN not acknowledged", testISS + 1, tcp.StateClosing},
		{"SEG.ACK = SND.NXT: FIN acknowledged", testISS + 2, tcp.StateTimeWait},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := toClosing(t, s)

			reply := handle(t, s, incomingIP(), incomingTCP(tcp.FlagACK, clientISN+2, tt.ack), nil)

			if reply != nil {
				t.Errorf("reply = %x, want nil", reply)
			}
			checkState(t, c, tt.wantState)
		})
	}
}

func TestHandle_LastAck_ACK(t *testing.T) {
	// Fifth、LAST-ACK: 自分の FIN が ACK されたら、TCB を消して CLOSED にする。
	// 相手の FIN には CLOSE-WAIT で ACK を返し終えているので、TIME-WAIT は挟まない
	tests := []struct {
		name        string
		ack         uint32
		wantState   tcp.State
		wantInTable bool
	}{
		{"SEG.ACK = SND.UNA: FIN not acknowledged", testISS + 1, tcp.StateLastAck, true},
		{"SEG.ACK = SND.NXT: FIN acknowledged", testISS + 2, tcp.StateClosed, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := toLastAck(t, s)

			reply := handle(t, s, incomingIP(), incomingTCP(tcp.FlagACK, clientISN+2, tt.ack), nil)

			if reply != nil {
				t.Errorf("reply = %x, want nil", reply)
			}
			checkState(t, c, tt.wantState)
			if _, ok := s.conns[testKey()]; ok != tt.wantInTable {
				t.Errorf("connection in the table = %v, want %v", ok, tt.wantInTable)
			}
		})
	}
}

func TestClose_CloseWait_UnsentData_FINWaitsUntilDataIsSent(t *testing.T) {
	// CLOSE-WAIT で、送れないデータが残ったまま Close した。状態は先に LAST-ACK になる
	// (docs/design.md の「対話で決めた仕様」)。データへの ACK で残りと FIN を送り、
	// FIN への ACK が来て初めて CLOSED になる
	s := newTestStack()
	c := establishWithWindow(t, s, 5)
	fin := incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+1, testISS+1)
	fin.Window = 5
	s.Handle(incomingIP(), fin, nil)
	takeSent(t, s) // FIN への ACK
	mustState(t, c, tcp.StateCloseWait)
	c.Write([]byte("hello world"))
	takeSent(t, s) // "hello"

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if segs := takeSent(t, s); len(segs) != 0 {
		t.Fatalf("Close sent %d segments, want 0 (unsent data remains)", len(segs))
	}
	checkState(t, c, tcp.StateLastAck)

	ack := incomingTCP(tcp.FlagACK, clientISN+2, testISS+6)
	ack.Window = 100
	s.Handle(incomingIP(), ack, nil)

	segs := takeSent(t, s)
	if len(segs) != 2 {
		t.Fatalf("sent %d segments after the ACK, want 2 (data, FIN)", len(segs))
	}
	if string(segs[0].payload) != " world" {
		t.Errorf("payload = %q, want %q", segs[0].payload, " world")
	}
	checkSegment(t, segs[1].hdr, tcp.FlagFIN|tcp.FlagACK, testISS+12, clientISN+2)
	// この ACK の時点では FIN を送っていなかったので、CLOSED にはならない
	checkState(t, c, tcp.StateLastAck)

	s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+2, testISS+13), nil)
	checkState(t, c, tcp.StateClosed)
	if _, ok := s.conns[testKey()]; ok {
		t.Error("connection still in the table after the ACK of our FIN")
	}
}

// ---------------------------------------------------------------------------
// T7-4: 自分から閉じたあとに受けるデータと FIN (RFC 9293 3.10.7.4 Seventh、Eighth)
// ---------------------------------------------------------------------------

func TestHandle_FinWait_Data_Received(t *testing.T) {
	// Seventh: FIN-WAIT-1 と FIN-WAIT-2 では、ESTABLISHED と同じようにデータを受け取る。
	// Close は「もう送らない」であって、「もう受け取らない」ではない (3.6)
	for _, tt := range []stateSetup{
		{"FIN-WAIT-1", toFinWait1},
		{"FIN-WAIT-2", toFinWait2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := tt.setup(t, s)
			state := c.State

			th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagPSH|tcp.FlagACK, clientISN+1, c.Snd.UNA), []byte("hello")))

			checkSegment(t, th, tcp.FlagACK, testISS+2, clientISN+6)
			if got := readAll(c); got != "hello" {
				t.Errorf("Read = %q, want %q", got, "hello")
			}
			checkState(t, c, state)
		})
	}
}

func TestHandle_FinWait1_FIN_EntersClosing(t *testing.T) {
	// Eighth、FIN-WAIT-1: 自分の FIN がまだ ACK されていなければ CLOSING に入る (両側が同時に閉じた)。
	// 相手の FIN の SEG.ACK = testISS+1 は、こちらの FIN (testISS+1) を含まない
	s := newTestStack()
	c := toFinWait1(t, s)

	th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+1, testISS+1), nil))

	checkSegment(t, th, tcp.FlagACK, testISS+2, clientISN+2)
	checkState(t, c, tcp.StateClosing)
}

func TestHandle_FinWait1_FINWithACKOfOurFIN_EntersTimeWait(t *testing.T) {
	// Eighth、FIN-WAIT-1: "If our FIN has been ACKed (perhaps in this segment), then enter TIME-WAIT"。
	// 相手の FIN|ACK が、こちらの FIN への ACK も運んでいる (SEG.ACK = testISS+2)。
	// 3.3.2 の Figure 5 にはない遷移 (Note 2)
	s := newTestStack()
	c := toFinWait1(t, s)

	th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+1, testISS+2), nil))

	checkSegment(t, th, tcp.FlagACK, testISS+2, clientISN+2)
	checkState(t, c, tcp.StateTimeWait)
}

func TestHandle_FinWait2_FIN_EntersTimeWait(t *testing.T) {
	// Eighth、FIN-WAIT-2: 相手の FIN に ACK を返し、TIME-WAIT に入る
	s := newTestStack()
	c := toFinWait2(t, s)

	th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+1, testISS+2), nil))

	checkSegment(t, th, tcp.FlagACK, testISS+2, clientISN+2)
	if c.Rcv.NXT != clientISN+2 {
		t.Errorf("Rcv.NXT = %d, want %d", c.Rcv.NXT, clientISN+2)
	}
	checkState(t, c, tcp.StateTimeWait)
	if s.conns[testKey()] != c {
		t.Error("connection removed from the table, want it kept during TIME-WAIT")
	}
}

func TestHandle_FinWait2_DataAndFIN_OneACK(t *testing.T) {
	// 相手の最後のデータに FIN が付いている。データを受け取り、まとめて 1 つの ACK を返して TIME-WAIT に入る
	s := newTestStack()
	c := toFinWait2(t, s)

	s.Handle(incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagPSH|tcp.FlagACK, clientISN+1, testISS+2), []byte("hello"))

	segs := takeSent(t, s)
	if len(segs) != 1 {
		t.Fatalf("sent %d segments, want 1", len(segs))
	}
	checkSegment(t, segs[0].hdr, tcp.FlagACK, testISS+2, clientISN+1+5+1)
	if got := readAll(c); got != "hello" {
		t.Errorf("Read = %q, want %q", got, "hello")
	}
	checkState(t, c, tcp.StateTimeWait)
}

func TestHandle_Closing_UnsentData_SentBeforeFIN(t *testing.T) {
	// 送れないデータが残ったまま Close し、FIN を送る前に相手の FIN が届いた。CLOSING に入り、
	// ウィンドウが開いたら残りのデータと FIN を送る。FIN への ACK で TIME-WAIT に入る
	s := newTestStack()
	c := establishWithWindow(t, s, 5)
	c.Write([]byte("hello world"))
	c.Close()
	takeSent(t, s) // "hello"
	mustState(t, c, tcp.StateFinWait1)

	fin := incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+1, testISS+1)
	fin.Window = 5
	s.Handle(incomingIP(), fin, nil)
	if segs := takeSent(t, s); len(segs) != 1 {
		t.Fatalf("sent %d segments for the peer's FIN, want 1 (ACK)", len(segs))
	}
	checkState(t, c, tcp.StateClosing)

	ack := incomingTCP(tcp.FlagACK, clientISN+2, testISS+6)
	ack.Window = 100
	s.Handle(incomingIP(), ack, nil)
	segs := takeSent(t, s)
	if len(segs) != 2 {
		t.Fatalf("sent %d segments after the ACK, want 2 (data, FIN)", len(segs))
	}
	if string(segs[0].payload) != " world" {
		t.Errorf("payload = %q, want %q", segs[0].payload, " world")
	}
	checkSegment(t, segs[1].hdr, tcp.FlagFIN|tcp.FlagACK, testISS+12, clientISN+2)
	checkState(t, c, tcp.StateClosing)

	s.Handle(incomingIP(), incomingTCP(tcp.FlagACK, clientISN+2, testISS+13), nil)
	checkState(t, c, tcp.StateTimeWait)
}

func TestHandle_AfterPeerFIN_RetransmittedFIN_ReACKed(t *testing.T) {
	// こちらの ACK が届かず、相手が FIN を再送した。First で受理できないので、いまの RCV.NXT を ACK で返す。
	// 状態は変えない
	for _, tt := range afterPeerFIN {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := tt.setup(t, s)
			state := c.State

			th, _ := mustReply(t, handle(t, s, incomingIP(), incomingTCP(tcp.FlagFIN|tcp.FlagACK, clientISN+1, c.Snd.UNA), nil))

			checkSegment(t, th, tcp.FlagACK, testISS+2, clientISN+2)
			checkState(t, c, state)
		})
	}
}

func TestHandle_AfterPeerFIN_DataIgnored(t *testing.T) {
	// Seventh: 相手の FIN を受けたあとの状態では、データを無視する。返信もしない
	for _, tt := range afterPeerFIN {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := tt.setup(t, s)
			state := c.State

			reply := handle(t, s, incomingIP(), incomingTCP(tcp.FlagPSH|tcp.FlagACK, clientISN+2, c.Snd.UNA), []byte("hello"))

			if reply != nil {
				t.Errorf("reply = %x, want nil", reply)
			}
			if c.Rcv.NXT != clientISN+2 {
				t.Errorf("Rcv.NXT = %d, want %d (unchanged)", c.Rcv.NXT, clientISN+2)
			}
			if got := readAll(c); got != "" {
				t.Errorf("Read = %q, want empty", got)
			}
			checkState(t, c, state)
		})
	}
}

func TestHandle_TimeWait_PureACK_NoReply(t *testing.T) {
	// TIME-WAIT で受理できる ACK だけのセグメントには、何も返さない。
	// 両側が TIME-WAIT のとき (同時に閉じた)、ACK に ACK を返すと応酬が止まらなくなる
	s := newTestStack()
	c := toTimeWait(t, s)

	reply := handle(t, s, incomingIP(), incomingTCP(tcp.FlagACK, clientISN+2, testISS+2), nil)

	if reply != nil {
		t.Errorf("reply = %x, want nil", reply)
	}
	checkState(t, c, tcp.StateTimeWait)
}

func TestHandle_RST_ClosesFromClosingStates(t *testing.T) {
	// Second: 同期済みのどの状態でも、ウィンドウ内の RST を受けたら CLOSED にして TCB を消す。返信はしない
	for _, tt := range []stateSetup{
		{"FIN-WAIT-1", toFinWait1},
		{"FIN-WAIT-2", toFinWait2},
		{"CLOSING", toClosing},
		{"LAST-ACK", toLastAck},
		{"TIME-WAIT", toTimeWait},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStack()
			c := tt.setup(t, s)

			reply := handle(t, s, incomingIP(), incomingTCP(tcp.FlagRST, c.Rcv.NXT, 0), nil)

			if reply != nil {
				t.Errorf("reply = %x, want nil", reply)
			}
			checkState(t, c, tcp.StateClosed)
			if _, ok := s.conns[testKey()]; ok {
				t.Error("connection still in the table after RST")
			}
		})
	}
}

package stack

import (
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

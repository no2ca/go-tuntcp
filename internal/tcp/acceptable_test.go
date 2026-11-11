package tcp

import "testing"

// RFC 9293 3.10.7.4 First, Table 6: Segment Acceptability Tests
//
//	SEG.LEN  RCV.WND  受理する条件
//	0        0        SEG.SEQ = RCV.NXT
//	0        >0       RCV.NXT =< SEG.SEQ < RCV.NXT+RCV.WND
//	>0       0        受理しない
//	>0       >0       RCV.NXT =< SEG.SEQ < RCV.NXT+RCV.WND
//	                  または RCV.NXT =< SEG.SEQ+SEG.LEN-1 < RCV.NXT+RCV.WND
//
// 表の 4 行それぞれについて、条件の境界 (ちょうど端、1 つ外) を確かめる。
func TestSegmentAcceptable(t *testing.T) {
	const (
		nxt = 1000 // RCV.NXT
		wnd = 100  // RCV.WND。ウィンドウは 1000〜1099
	)
	tests := []struct {
		name   string
		seq    uint32
		segLen uint32
		rcvNXT uint32
		rcvWND uint16
		want   bool
	}{
		// 1 行目: SEG.LEN = 0, RCV.WND = 0
		{"len 0, wnd 0: SEQ = RCV.NXT", nxt, 0, nxt, 0, true},
		{"len 0, wnd 0: SEQ = RCV.NXT+1", nxt + 1, 0, nxt, 0, false},
		{"len 0, wnd 0: SEQ = RCV.NXT-1", nxt - 1, 0, nxt, 0, false},

		// 2 行目: SEG.LEN = 0, RCV.WND > 0
		{"len 0: ウィンドウの先頭", nxt, 0, nxt, wnd, true},
		{"len 0: ウィンドウの先頭の 1 つ前", nxt - 1, 0, nxt, wnd, false},
		{"len 0: ウィンドウの最後", nxt + wnd - 1, 0, nxt, wnd, true},
		{"len 0: ウィンドウの最後の 1 つ後", nxt + wnd, 0, nxt, wnd, false},

		// 3 行目: SEG.LEN > 0, RCV.WND = 0
		{"len 1, wnd 0: SEQ = RCV.NXT でも受理しない", nxt, 1, nxt, 0, false},

		// 4 行目: SEG.LEN > 0, RCV.WND > 0
		{"len 10: ウィンドウの先頭から始まる", nxt, 10, nxt, wnd, true},
		{"len 10: 最後の 1 バイトだけがウィンドウに入る", nxt - 9, 10, nxt, wnd, true},
		{"len 10: 全部がウィンドウより前", nxt - 10, 10, nxt, wnd, false},
		{"len 10: 最初の 1 バイトだけがウィンドウに入る", nxt + wnd - 1, 10, nxt, wnd, true},
		{"len 10: 全部がウィンドウより後", nxt + wnd, 10, nxt, wnd, false},
		{"len 200: ウィンドウの前から後ろまでを覆う", nxt - 50, 200, nxt, wnd, false},

		// 2^32 の折り返し。ウィンドウは 0xfffffff0〜0xffffffff と 0〜0x53
		{"折り返し, len 0: 折り返す前", 0xfffffff5, 0, 0xfffffff0, wnd, true},
		{"折り返し, len 0: 折り返した後", 0x10, 0, 0xfffffff0, wnd, true},
		{"折り返し, len 0: ウィンドウの最後", 0x53, 0, 0xfffffff0, wnd, true},
		{"折り返し, len 0: ウィンドウの最後の 1 つ後", 0x54, 0, 0xfffffff0, wnd, false},
		{"折り返し, len 0: ウィンドウの先頭の 1 つ前", 0xffffffef, 0, 0xfffffff0, wnd, false},
		{"折り返し, len 10: 最後の 1 バイトだけがウィンドウに入る", 0xffffffe7, 10, 0xfffffff0, wnd, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SegmentAcceptable(tt.seq, tt.segLen, tt.rcvNXT, tt.rcvWND)
			if got != tt.want {
				t.Errorf("SegmentAcceptable(seq=%#x, len=%d, RCV.NXT=%#x, RCV.WND=%d) = %v, want %v",
					tt.seq, tt.segLen, tt.rcvNXT, tt.rcvWND, got, tt.want)
			}
		})
	}
}

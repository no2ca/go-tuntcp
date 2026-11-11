package tcp

// a < b
func SeqLT(a, b uint32) bool { return int32(a-b) < 0 }

// a <= b
func SeqLE(a, b uint32) bool { return int32(a-b) <= 0 }

// SegmentAcceptable は、受信したセグメントが受信ウィンドウに掛かっているかを判定する。
// RFC 9293 3.10.7.4 First の Table 6 (Segment Acceptability Tests) をそのまま写したもの。
//
// 接続のあるセグメントを処理するとき、最初に呼ぶ。古い重複やウィンドウの外のセグメントを
// ここで落とし、後ろの段階 (RST、SYN、ACK、データ) が見るのは受理できるものだけにする。
//
// segLen は SEG.LEN。SYN と FIN もそれぞれ 1 と数える (RFC 9293 3.4)。
func SegmentAcceptable(seq, segLen, rcvNXT uint32, rcvWND uint16) bool {
	return true // 未実装: すべて受理する
}

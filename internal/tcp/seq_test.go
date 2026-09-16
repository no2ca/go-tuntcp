package tcp

import "testing"

// RFC 9293 3.4: シーケンス番号の比較は modulo 2^32 で行う。
// 0xfffffff0 の「後」は 0x10 なので、素朴な < では判定を誤る。
func TestSeqCompare(t *testing.T) {
	tests := []struct {
		name string
		a, b uint32
		lt   bool // a < b
		le   bool // a <= b
	}{
		{"0 < 1", 0, 1, true, true},
		{"1 > 0", 1, 0, false, false},
		{"equal", 5, 5, false, true},
		{"equal at max", 0xffffffff, 0xffffffff, false, true},

		// 巻き戻りをまたぐ
		{"0xfffffff0 < 0x10 (wrapped)", 0xfffffff0, 0x10, true, true},
		{"0x10 > 0xfffffff0 (wrapped)", 0x10, 0xfffffff0, false, false},
		{"0xffffffff < 0 (wrapped)", 0xffffffff, 0, true, true},
		{"0 > 0xffffffff (wrapped)", 0, 0xffffffff, false, false},

		// 距離が 2^31 未満なら「前」、それ以上なら「後ろ」とみなす
		{"distance 2^31-1 is ahead", 0, 0x7fffffff, true, true},
		{"distance 2^31+1 is behind", 0, 0x80000001, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SeqLT(tt.a, tt.b); got != tt.lt {
				t.Errorf("SeqLT(%#x, %#x) = %v, want %v", tt.a, tt.b, got, tt.lt)
			}
			if got := SeqLE(tt.a, tt.b); got != tt.le {
				t.Errorf("SeqLE(%#x, %#x) = %v, want %v", tt.a, tt.b, got, tt.le)
			}
		})
	}
}

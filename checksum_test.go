package main

import "testing"

func TestSum(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
		want uint32
	}{
		{"empty", nil, 0},
		{"single word", []byte{0x12, 0x34}, 0x1234},
		{"two words", []byte{0x12, 0x34, 0x00, 0x01}, 0x1235},
		// 奇数長: 末尾バイトは上位 8bit として足す (0x0102 + 0x0300)
		{"odd length pads trailing byte", []byte{0x01, 0x02, 0x03}, 0x0402},
		// 16bit を超えてもここでは畳み込まない
		{"overflows 16 bits", []byte{0xff, 0xff, 0xff, 0xff}, 0x1fffe},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Sum(tt.buf); got != tt.want {
				t.Errorf("Sum(%x) = %#x, want %#x", tt.buf, got, tt.want)
			}
		})
	}
}

func TestFold(t *testing.T) {
	tests := []struct {
		name string
		s    uint32
		want uint16
	}{
		{"zero", 0, 0xffff},
		{"no carry", 0x1234, ^uint16(0x1234)},
		// 0xffff + 0x1 = 0x10000 -> さらに畳んで 0x0001
		{"single carry", 0x1ffff, ^uint16(0x0001)},
		{"carry produces another carry", 0xffff0001, ^uint16(0x0001)},
		{"all ones", 0xffffffff, 0x0000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Fold(tt.s); got != tt.want {
				t.Errorf("Fold(%#x) = %#x, want %#x", tt.s, got, tt.want)
			}
		})
	}
}

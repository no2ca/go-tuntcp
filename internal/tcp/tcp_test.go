package tcp

import (
	"bytes"
	"testing"
)

var (
	tcpTestSrc = [4]byte{10, 10, 0, 1}
	tcpTestDst = [4]byte{10, 10, 0, 2}
)

// SrcPort 12345 -> DstPort 80, Seq 1, Ack 0, DataOffset 5, SYN, Window 0xffff。
// チェックサム 0x6b42 は擬似ヘッダ (10.10.0.1 -> 10.10.0.2, TCP, len 20) 込みで
// Go 実装とは別に Python で計算した値。
var validHeader = []byte{
	0x30, 0x39, 0x00, 0x50,
	0x00, 0x00, 0x00, 0x01,
	0x00, 0x00, 0x00, 0x00,
	0x50, 0x02, 0xff, 0xff,
	0x6b, 0x42, 0x00, 0x00,
}

var validStruct = Header{
	SrcPort:    12345,
	DstPort:    80,
	Seq:        1,
	Ack:        0,
	DataOffset: 5,
	Flags:      FlagSYN,
	Window:     0xffff,
	Checksum:   0x6b42,
	Urgent:     0,
}

func TestParseTCPHeader(t *testing.T) {
	hdr, options, payload, err := Parse(tcpTestSrc, tcpTestDst, validHeader)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if hdr != validStruct {
		t.Errorf("header = %+v, want %+v", hdr, validStruct)
	}
	if len(options) != 0 {
		t.Errorf("options = %x, want empty", options)
	}
	if len(payload) != 0 {
		t.Errorf("payload = %x, want empty", payload)
	}
}

func TestParseTCPHeader_WithPayload(t *testing.T) {
	// 同じヘッダに "hello" を付けたもの。チェックサムはペイロードと擬似ヘッダの長さも含むので変わる。
	seg := append([]byte{}, validHeader...)
	seg[16], seg[17] = 0x27, 0x6b
	seg = append(seg, []byte("hello")...)

	hdr, _, payload, err := Parse(tcpTestSrc, tcpTestDst, seg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if hdr.Checksum != 0x276b {
		t.Errorf("Checksum = %#x, want 0x276b", hdr.Checksum)
	}
	if !bytes.Equal(payload, []byte("hello")) {
		t.Errorf("payload = %q, want %q", payload, "hello")
	}
}

func TestParseTCPHeader_WithOptions(t *testing.T) {
	// DataOffset 6 (24 byte): MSS オプション (kind 2, len 4, 1460) を付ける。
	seg := append([]byte{}, validHeader...)
	seg[12] = 0x60
	seg = append(seg, 0x02, 0x04, 0x05, 0xb4)
	seg = append(seg, []byte("data")...)
	// チェックサムはテスト内で再計算する (値の正しさ自体は TestCalculateTCPChecksum で担保)
	seg[16], seg[17] = 0, 0
	cs := calculateChecksum(tcpTestSrc, tcpTestDst, seg)
	seg[16], seg[17] = byte(cs>>8), byte(cs)

	hdr, options, payload, err := Parse(tcpTestSrc, tcpTestDst, seg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if hdr.DataOffset != 6 {
		t.Errorf("DataOffset = %d, want 6", hdr.DataOffset)
	}
	if !bytes.Equal(options, []byte{0x02, 0x04, 0x05, 0xb4}) {
		t.Errorf("options = %x, want 020405b4", options)
	}
	if !bytes.Equal(payload, []byte("data")) {
		t.Errorf("payload = %q, want %q", payload, "data")
	}
}

func TestParseTCPHeader_Errors(t *testing.T) {
	mutate := func(i int, b byte) []byte {
		seg := append([]byte{}, validHeader...)
		seg[i] = b
		return seg
	}

	tests := []struct {
		name string
		src  [4]byte
		dst  [4]byte
		seg  []byte
	}{
		{"too short", tcpTestSrc, tcpTestDst, validHeader[:19]},
		{"checksum mismatch", tcpTestSrc, tcpTestDst, mutate(16, 0x00)},
		{"corrupted flags break checksum", tcpTestSrc, tcpTestDst, mutate(13, 0x12)},
		// 擬似ヘッダに含まれる IP アドレスが違えばチェックサムも合わない
		{"wrong pseudo header", [4]byte{10, 10, 0, 9}, tcpTestDst, validHeader},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, _, err := Parse(tt.src, tt.dst, tt.seg); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestParseTCPHeader_DataOffsetErrors(t *testing.T) {
	// DataOffset の検証はチェックサム検証の後なので、チェックサムを合わせ直してから試す。
	withOffset := func(off byte) []byte {
		seg := append([]byte{}, validHeader...)
		seg[12] = off << 4
		seg[16], seg[17] = 0, 0
		cs := calculateChecksum(tcpTestSrc, tcpTestDst, seg)
		seg[16], seg[17] = byte(cs>>8), byte(cs)
		return seg
	}

	tests := []struct {
		name string
		off  byte
	}{
		{"data offset below 5", 4},
		{"data offset exceeds segment", 15},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, _, err := Parse(tcpTestSrc, tcpTestDst, withOffset(tt.off)); err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestSerializeTCPHeader(t *testing.T) {
	// validStruct を Serialize すると validHeader と一致するはず。
	// Checksum フィールドの値は無視して再計算されるので、あえて違う値を入れておく。
	h := validStruct
	h.Checksum = 0xdead
	got := h.Serialize(tcpTestSrc, tcpTestDst, nil)
	if !bytes.Equal(got, validHeader) {
		t.Errorf("Serialize =\n%x, want\n%x", got, validHeader)
	}
}

func TestSerializeTCPHeader_WithPayload(t *testing.T) {
	// TestParseTCPHeader_WithPayload と同じセグメント ("hello" 付き、チェックサム 0x276b)。
	want := append([]byte{}, validHeader...)
	want[16], want[17] = 0x27, 0x6b
	want = append(want, []byte("hello")...)

	got := validStruct.Serialize(tcpTestSrc, tcpTestDst, []byte("hello"))
	if !bytes.Equal(got, want) {
		t.Errorf("Serialize =\n%x, want\n%x", got, want)
	}
}

func TestSerializeTCPHeader_Fields(t *testing.T) {
	// 各フィールドが正しいオフセット・バイトオーダーで書かれているか、境界値で確認する。
	h := Header{
		SrcPort:    0xffff,
		DstPort:    0x0001,
		Seq:        0x01020304,
		Ack:        0xfffffffe,
		DataOffset: 5,
		Flags:      FlagURG | FlagACK | FlagPSH | FlagRST | FlagSYN | FlagFIN,
		Window:     0x1234,
		Urgent:     0xabcd,
	}
	got := h.Serialize(tcpTestSrc, tcpTestDst, nil)

	want := []byte{
		0xff, 0xff, 0x00, 0x01,
		0x01, 0x02, 0x03, 0x04,
		0xff, 0xff, 0xff, 0xfe,
		0x50, 0x3f, 0x12, 0x34,
		0x00, 0x00, 0xab, 0xcd,
	}
	// チェックサムは calculateChecksum で埋める (値の正しさは TestCalculateTCPChecksum で担保)。
	cs := calculateChecksum(tcpTestSrc, tcpTestDst, want)
	want[16], want[17] = byte(cs>>8), byte(cs)

	if !bytes.Equal(got, want) {
		t.Errorf("Serialize =\n%x, want\n%x", got, want)
	}
}

func TestSerializeTCPHeader_RoundTrip(t *testing.T) {
	// Serialize -> Parse で元のヘッダとペイロードに戻ること。
	// Parse 側でチェックサム検証を通る = Serialize が正しいチェックサムを書いている。
	h := Header{
		SrcPort:    443,
		DstPort:    54321,
		Seq:        0xdeadbeef,
		Ack:        0xcafebabe,
		DataOffset: 5,
		Flags:      FlagACK | FlagPSH,
		Window:     65535,
		Urgent:     0,
	}
	payload := []byte("round trip")

	seg := h.Serialize(tcpTestSrc, tcpTestDst, payload)

	parsed, options, gotPayload, err := Parse(tcpTestSrc, tcpTestDst, seg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Parse は Checksum を埋めて返すので、比較用にコピーしておく。
	h.Checksum = parsed.Checksum
	if parsed != h {
		t.Errorf("header = %+v, want %+v", parsed, h)
	}
	if len(options) != 0 {
		t.Errorf("options = %x, want empty", options)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Errorf("payload = %q, want %q", gotPayload, payload)
	}
}

func TestSerializeTCPHeader_DoesNotAliasPayload(t *testing.T) {
	// 返り値は新しいバッファなので、呼び出し元の payload を書き換えても影響しない。
	payload := []byte("abc")
	seg := validStruct.Serialize(tcpTestSrc, tcpTestDst, payload)
	payload[0] = 'z'
	if !bytes.Equal(seg[20:], []byte("abc")) {
		t.Errorf("payload in segment = %q, want %q", seg[20:], "abc")
	}
}

func TestCalculateTCPChecksum(t *testing.T) {
	seg := append([]byte{}, validHeader...)
	seg[16], seg[17] = 0, 0
	if got := calculateChecksum(tcpTestSrc, tcpTestDst, seg); got != 0x6b42 {
		t.Errorf("calculateChecksum = %#x, want 0x6b42", got)
	}

	// 奇数長セグメント (ペイロード "hello")
	seg = append(seg, []byte("hello")...)
	if got := calculateChecksum(tcpTestSrc, tcpTestDst, seg); got != 0x276b {
		t.Errorf("calculateChecksum (odd length) = %#x, want 0x276b", got)
	}
}

func TestVerifyTCPChecksum(t *testing.T) {
	if !VerifyChecksum(tcpTestSrc, tcpTestDst, validHeader) {
		t.Error("valid segment reported as invalid")
	}
	// 擬似ヘッダの src/dst は足し算なので入れ替えても検出できない (1 の補数和の性質)。
	// 別のアドレスなら検出できる。
	if VerifyChecksum([4]byte{10, 10, 0, 9}, tcpTestDst, validHeader) {
		t.Error("segment with a different source address must not verify")
	}
}

func TestHeader_HasFlag(t *testing.T) {
	h := Header{Flags: FlagSYN | FlagACK}
	if !h.HasFlag(FlagSYN) || !h.HasFlag(FlagACK) {
		t.Error("SYN and ACK should be set")
	}
	if h.HasFlag(FlagFIN) || h.HasFlag(FlagRST) || h.HasFlag(FlagPSH) || h.HasFlag(FlagURG) {
		t.Error("unexpected flag reported as set")
	}
}

func TestHeader_StringFlags(t *testing.T) {
	tests := []struct {
		flags Flag
		want  string
	}{
		{0, "None"},
		{FlagSYN, "SYN"},
		{FlagSYN | FlagACK, "ACK | SYN"},
		{FlagFIN | FlagPSH | FlagACK, "ACK | PSH | FIN"},
		{FlagURG | FlagACK | FlagPSH | FlagRST | FlagSYN | FlagFIN, "URG | ACK | PSH | RST | SYN | FIN"},
	}
	for _, tt := range tests {
		h := Header{Flags: tt.flags}
		if got := h.StringFlags(); got != tt.want {
			t.Errorf("StringFlags(%#x) = %q, want %q", tt.flags, got, tt.want)
		}
	}
}

func TestState_String(t *testing.T) {
	tests := []struct {
		state State
		want  string
	}{
		{StateClosed, "CLOSED"},
		{StateListen, "LISTEN"},
		{StateSynSent, "SYN-SENT"},
		{StateSynReceived, "SYN-RECEIVED"},
		{StateEstablished, "ESTABLISHED"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

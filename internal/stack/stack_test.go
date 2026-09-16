package stack

import (
	"net/netip"
	"testing"

	"go-tuntcp/internal/ipv4"
	"go-tuntcp/internal/tcp"
)

var (
	peerAddr = netip.MustParseAddr("10.10.0.1") // 送ってきた側 (curl)
	ourAddr  = netip.MustParseAddr("10.10.0.2") // 自分 (TUN)
)

// 受信した IPv4 ヘッダ。buildRST は Src/Dst を入れ替えて返すはず。
func incomingIP() ipv4.Header {
	return ipv4.Header{
		Version:  4,
		IHL:      5,
		TotalLen: 40,
		ID:       0x1234,
		TTL:      64,
		Protocol: ipv4.ProtoTCP,
		Src:      peerAddr,
		Dst:      ourAddr,
	}
}

// 受信した TCP ヘッダ。フラグと Seq/Ack だけテストケースごとに差し替える。
func incomingTCP(flags tcp.Flag, seq, ack uint32) tcp.Header {
	return tcp.Header{
		SrcPort:    54321,
		DstPort:    80,
		Seq:        seq,
		Ack:        ack,
		DataOffset: 5,
		Flags:      flags,
		Window:     0xffff,
	}
}

// buildRST の返り値 (IPv4 パケット全体) を両方のレイヤでパースする。
// Parse がチェックサムも検証するので、ここを通れば送信しても "incorrect" にはならない。
func parseReply(t *testing.T, pkt []byte) (ipv4.Header, tcp.Header, []byte) {
	t.Helper()

	ip, ipPayload, err := ipv4.Parse(pkt)
	if err != nil {
		t.Fatalf("ipv4.Parse: %v", err)
	}
	if ip.Protocol != ipv4.ProtoTCP {
		t.Fatalf("Protocol = %d, want %d (TCP)", ip.Protocol, ipv4.ProtoTCP)
	}

	th, _, tcpPayload, err := tcp.Parse(ip.Src.As4(), ip.Dst.As4(), ipPayload)
	if err != nil {
		t.Fatalf("tcp.Parse: %v", err)
	}
	return ip, th, tcpPayload
}

func TestBuildRST(t *testing.T) {
	// RFC 9293 3.10.7.1 (CLOSED STATE)
	tests := []struct {
		name       string
		in         tcp.Header
		payloadLen int

		wantSeq   uint32
		wantAck   uint32
		wantFlags tcp.Flag
	}{
		{
			// ACK があるなら <SEQ=SEG.ACK><CTL=RST>
			name:      "ACK set: Seq = SEG.ACK, flags RST",
			in:        incomingTCP(tcp.FlagACK, 1000, 5000),
			wantSeq:   5000,
			wantAck:   0,
			wantFlags: tcp.FlagRST,
		},
		{
			// ACK が無いなら <SEQ=0><ACK=SEG.SEQ+SEG.LEN><CTL=RST,ACK>
			// SYN は SEG.LEN として 1 を占める (RFC 9293 3.4)。curl の最初の SYN に対する応答がこれ。
			name:      "SYN without ACK: Seq = 0, Ack = SEG.SEQ + 1, flags RST|ACK",
			in:        incomingTCP(tcp.FlagSYN, 1000, 0),
			wantSeq:   0,
			wantAck:   1001,
			wantFlags: tcp.FlagRST | tcp.FlagACK,
		},
		{
			// ACK 無し + ペイロードあり: Ack = SEG.SEQ + payloadLen
			name:       "data without ACK: Ack covers payload",
			in:         incomingTCP(tcp.FlagPSH, 1000, 0),
			payloadLen: 10,
			wantSeq:    0,
			wantAck:    1010,
			wantFlags:  tcp.FlagRST | tcp.FlagACK,
		},
		{
			// SYN と FIN の両方が立っていればそれぞれ 1 ずつ足す
			name:       "SYN+FIN with payload: Ack = SEG.SEQ + payloadLen + 2",
			in:         incomingTCP(tcp.FlagSYN|tcp.FlagFIN, 1000, 0),
			payloadLen: 10,
			wantSeq:    0,
			wantAck:    1012,
			wantFlags:  tcp.FlagRST | tcp.FlagACK,
		},
		{
			// ACK があればペイロードや SYN があっても Seq = SEG.ACK で Ack は使わない
			name:       "ACK with payload: Ack is not used",
			in:         incomingTCP(tcp.FlagACK|tcp.FlagPSH, 1000, 5000),
			payloadLen: 10,
			wantSeq:    5000,
			wantAck:    0,
			wantFlags:  tcp.FlagRST,
		},
		{
			// Seq が uint32 の端で wrap する
			name:      "Ack wraps around uint32",
			in:        incomingTCP(tcp.FlagSYN, 0xffffffff, 0),
			wantSeq:   0,
			wantAck:   0,
			wantFlags: tcp.FlagRST | tcp.FlagACK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkt := BuildRST(incomingIP(), tt.in, tt.payloadLen)
			if pkt == nil {
				t.Fatal("buildRST returned nil, want a packet")
			}

			ip, th, payload := parseReply(t, pkt)

			// IPv4: 宛先と送信元が入れ替わっている
			if ip.Src != ourAddr {
				t.Errorf("IP Src = %v, want %v", ip.Src, ourAddr)
			}
			if ip.Dst != peerAddr {
				t.Errorf("IP Dst = %v, want %v", ip.Dst, peerAddr)
			}
			if ip.TTL == 0 {
				t.Error("IP TTL = 0, packet would be dropped by the first router")
			}
			if ip.TotalLen != 40 {
				t.Errorf("IP TotalLen = %d, want 40 (20 IP + 20 TCP)", ip.TotalLen)
			}
			if len(pkt) != 40 {
				t.Errorf("len(pkt) = %d, want 40", len(pkt))
			}

			// TCP: ポートも入れ替わっている
			if th.SrcPort != tt.in.DstPort {
				t.Errorf("SrcPort = %d, want %d", th.SrcPort, tt.in.DstPort)
			}
			if th.DstPort != tt.in.SrcPort {
				t.Errorf("DstPort = %d, want %d", th.DstPort, tt.in.SrcPort)
			}
			if th.Seq != tt.wantSeq {
				t.Errorf("Seq = %d, want %d", th.Seq, tt.wantSeq)
			}
			if th.Ack != tt.wantAck {
				t.Errorf("Ack = %d, want %d", th.Ack, tt.wantAck)
			}
			if th.Flags != tt.wantFlags {
				t.Errorf("Flags = %s (%#x), want %#x", th.StringFlags(), th.Flags, tt.wantFlags)
			}
			if th.DataOffset != 5 {
				t.Errorf("DataOffset = %d, want 5", th.DataOffset)
			}
			if len(payload) != 0 {
				t.Errorf("payload = %x, want empty (RST carries no data)", payload)
			}
		})
	}
}

func TestBuildRST_IgnoresRST(t *testing.T) {
	// RST に RST で返すとお互いに投げ合ってループするので、何も返さない。
	tests := []struct {
		name string
		in   tcp.Header
	}{
		{"RST only", incomingTCP(tcp.FlagRST, 1000, 0)},
		{"RST|ACK", incomingTCP(tcp.FlagRST|tcp.FlagACK, 1000, 5000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if pkt := BuildRST(incomingIP(), tt.in, 0); pkt != nil {
				t.Errorf("buildRST = %x, want nil", pkt)
			}
		})
	}
}

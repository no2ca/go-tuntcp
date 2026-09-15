package tcp

//go:generate stringer -type=State -linecomment

// https://datatracker.ietf.org/doc/html/rfc9293#section-3.3.2
type State int

const (
	StateClosed      State = iota // CLOSED
	StateListen                   // LISTEN
	StateSynSent                  // SYN-SENT
	StateSynReceived              // SYN-RECEIVED
	StateEstablished              // ESTABLISHED
)

// SendSequenceSpace は RFC 9293 3.3.1 の Send Sequence Variables
type SendSequenceSpace struct {
	UNA uint32 // 未確認の最古のシーケンス番号
	NXT uint32 // 次に送信するシーケンス番号
	WND uint16 // 相手が広告した受信ウィンドウ
	UP  bool   // urgent pointer
	WL1 uint32 // 最後にウィンドウ更新を行ったセグメントの SEG.SEQ
	WL2 uint32 // 同じく SEG.ACK
	ISS uint32 // 初期送信シーケンス番号
}

// RecvSequenceSpace は RFC 9293 3.3.1 の Receive Sequence Variables
type RecvSequenceSpace struct {
	NXT uint32 // 次に受信を期待するシーケンス番号
	WND uint16 // 自分が広告している受信ウィンドウ
	UP  bool   // urgent pointer
	IRS uint32 // 初期受信シーケンス番号
}

type ControlBlock struct {
	State State
	Snd   SendSequenceSpace
	Rcv   RecvSequenceSpace
}

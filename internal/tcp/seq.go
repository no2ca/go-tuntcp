package tcp

// a < b
func SeqLT(a, b uint32) bool { return int32(a-b) < 0 }

// a <= b
func SeqLE(a, b uint32) bool { return int32(a-b) <= 0 }

package checksum

import "encoding/binary"

func Sum(buf []byte) uint32 {
	var sum uint32

	for i := 0; i+1 < len(buf); i += 2 {
		sum += uint32(binary.BigEndian.Uint16((buf[i : i+2])))
	}

	if len(buf)%2 == 1 {
		sum += uint32(buf[len(buf)-1]) << 8
	}

	return sum
}

func Fold(s uint32) uint16 {
	for s > 0xffff {
		s = (s & 0xffff) + (s >> 16)
	}
	return ^uint16(s)
}

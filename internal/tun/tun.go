package tun

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	IFF_TUN   = 0x0001 // EthernetではなくIPパケットを扱う設定
	IFF_NO_PI = 0x1000 // 4byteのメタデータのPIを先頭に付けない設定
	TUNSETIFF = 0x400454ca
)

type ifreq struct {
	Name  [16]byte
	Flags uint16
	_     [22]byte
}

func Create(name string) (*os.File, error) {
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var req ifreq

	copy(req.Name[:], name)
	req.Flags = IFF_TUN | IFF_NO_PI

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(TUNSETIFF), uintptr(unsafe.Pointer(&req)))

	if errno != 0 {
		f.Close()
		return nil, errno
	}

	return f, nil
}

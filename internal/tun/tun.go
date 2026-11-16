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

// Create は、TUN デバイス name を作り、読み書きできるファイルとして返す。
// 1 回の Read が IP パケットを 1 つ返し、1 回の Write が 1 つ受け取る。
//
// ノンブロッキングで開いてから os.NewFile に渡す。こうすると Go のランタイムがポーラーで待つようになり、
// Close したときに、待っている Read が返る (tuntcp.New がデバイスに求めていること)。
// 捨てた案: os.OpenFile で開き、ioctl のために File.Fd を呼ぶ (以前の形)。Fd はファイルをブロッキングに戻すので、
// Close しても、待っている Read は次のパケットが届くまで返らない (2026-11-16 に、名前空間の中で確かめた)。
func Create(name string) (*os.File, error) {
	fd, err := syscall.Open("/dev/net/tun", syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: "/dev/net/tun", Err: err}
	}
	var req ifreq

	copy(req.Name[:], name)
	req.Flags = IFF_TUN | IFF_NO_PI

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(TUNSETIFF), uintptr(unsafe.Pointer(&req)))

	if errno != 0 {
		syscall.Close(fd)
		return nil, errno
	}

	return os.NewFile(uintptr(fd), "/dev/net/tun"), nil
}

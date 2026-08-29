package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"unsafe"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT)
	defer stop()

	name := "tun0"
	tun, err := createTUN(name)
	if err != nil {
		log.Fatal(err)
	}

	defer func() {
		tun.Close()
		fmt.Printf("%s closed\n", name)
	}()

	fmt.Printf("created tun interface: %s\n", name)
	<-ctx.Done()
}

const (
	IFF_TUN   = 0x0001 // EthernetではなくIPパケットを扱う
	IFF_NO_PI = 0x1000 // 4byteのメタデータのPIを先頭に付けない
	TUNSETIFF = 0x400454ca
)

type ifreq struct {
	Name  [16]byte
	Flags uint16
	_     [22]byte
}

func createTUN(name string) (*os.File, error) {
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

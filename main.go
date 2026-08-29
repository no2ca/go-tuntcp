package main

import (
	"context"
	"fmt"
	"log"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
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

	go func() {
		buf := make([]byte, 1500)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				n, err := tun.Read(buf)
				if err != nil {
					return
				}
				fmt.Printf("========\n")
				fmt.Printf("received %d bytes: %  x\n", n, buf[:n])
				// IPv4: 1=ICMP, 6=TCP, 17=UDP
				fmt.Printf("proto=%d\n", buf[9])
				hdr, err := ParseIPv4Header(buf)
				if err != nil {
					return
				}
				fmt.Printf("Protocol: %v, Src: %v, Dst: %v\n", hdr.Protocol, hdr.Src, hdr.Dst)
			}
		}
	}()

	<-ctx.Done()
}

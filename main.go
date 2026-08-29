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
		defer fmt.Printf("Reading packets done. Waiting for an interrupt...")
		buf := make([]byte, 1500)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				n, err := tun.Read(buf)
				if err != nil {
					log.Print(err)
					return
				}

				fmt.Printf("========\n")
				fmt.Printf("received %d bytes: %  x\n", n, buf[:n])

				hdr, err := ParseIPv4Header(buf)
				if err != nil {
					log.Print(err)
				}
				// IPv4: 1=ICMP, 6=TCP, 17=UDP
				fmt.Printf("Protocol: %v, Src: %v, Dst: %v\n", hdr.Protocol, hdr.Src, hdr.Dst)
			}
		}
	}()

	<-ctx.Done()
}

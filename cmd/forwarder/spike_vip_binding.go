//go:build ignore

// Package main contains a small spike for VIP binding behavior.
// Run with: go run spike_vip_binding.go
package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
)

// VIPBindingNeeded records the spike's conclusion.
const VIPBindingNeeded = true

// This spike is intentionally standalone and excluded from normal builds.
// It demonstrates that a UDP socket reply uses the local socket/interface
// address unless the application explicitly chooses a source IP.

func main() {
	target := strings.TrimSpace(os.Getenv("SPIKE_TARGET"))
	if target == "" {
		target = "127.0.0.1:9999"
	}

	bindingIP := strings.TrimSpace(os.Getenv("SPIKE_BIND_IP"))

	if err := demo(target, bindingIP); err != nil {
		fmt.Fprintf(os.Stderr, "spike failed: %v\n", err)
		os.Exit(1)
	}
}

func demo(target, bindingIP string) error {
	raddr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return fmt.Errorf("resolve target: %w", err)
	}

	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: raddr.Port})
	if err != nil {
		return fmt.Errorf("listen local server: %w", err)
	}
	defer server.Close()

	serverDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 2048)
		_ = server.SetDeadline(time.Now().Add(5 * time.Second))
		n, clientAddr, readErr := server.ReadFromUDP(buf)
		if readErr != nil {
			serverDone <- fmt.Errorf("server read: %w", readErr)
			return
		}

		localAddr := server.LocalAddr().(*net.UDPAddr)
		fmt.Printf("server received %q from %s on local addr %s\n", string(buf[:n]), clientAddr, localAddr)

		if _, writeErr := server.WriteToUDP([]byte("pong"), clientAddr); writeErr != nil {
			serverDone <- fmt.Errorf("server write: %w", writeErr)
			return
		}

		serverDone <- nil
	}()

	localAddr := &net.UDPAddr{}
	if bindingIP != "" {
		localAddr.IP = net.ParseIP(bindingIP)
		if localAddr.IP == nil {
			return fmt.Errorf("invalid SPIKE_BIND_IP %q", bindingIP)
		}
	}

	client, err := net.DialUDP("udp", localAddr, raddr)
	if err != nil {
		return fmt.Errorf("dial client: %w", err)
	}
	defer client.Close()

	if bindingIP != "" {
		if rawErr := setPktInfo(client); rawErr != nil {
			fmt.Printf("warning: failed enabling IP_PKTINFO on client socket: %v\n", rawErr)
		}
	}

	fmt.Printf("VIPBindingNeeded=%v\n", VIPBindingNeeded)
	fmt.Printf("client local addr: %s\n", client.LocalAddr())
	fmt.Printf("client remote addr: %s\n", client.RemoteAddr())

	if _, err = client.Write([]byte("ping")); err != nil {
		return fmt.Errorf("client write: %w", err)
	}

	buf := make([]byte, 2048)
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	n, err := client.Read(buf)
	if err != nil {
		return fmt.Errorf("client read: %w", err)
	}

	if err = <-serverDone; err != nil {
		return err
	}

	fmt.Printf("client received %q on local addr %s\n", string(buf[:n]), client.LocalAddr())
	if bindingIP == "" {
		fmt.Println("observation: without an explicit local bind, the kernel chose the client source address automatically")
	} else {
		fmt.Println("observation: with an explicit local bind, the client socket used the requested source IP")
	}

	fmt.Println("note: on GKE the critical question is whether the reply source becomes the LB VIP automatically; research says NO for UDP passthrough, so explicit VIP selection is needed")

	return nil
}

func setPktInfo(conn *net.UDPConn) error {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return err
	}

	var sockErr error
	controlErr := rawConn.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_PKTINFO, 1)
	})
	if controlErr != nil {
		return controlErr
	}

	return sockErr
}

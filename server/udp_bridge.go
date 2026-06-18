package main

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

type udpBridge struct {
	conn      *net.UDPConn
	onInbound func([]byte)

	mu        sync.RWMutex
	esp32Addr *net.UDPAddr
}

func newUDPBridge(listenAddr string, onInbound func([]byte)) (*udpBridge, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve UDP addr %q: %w", listenAddr, err)
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("listen UDP on %q: %w", listenAddr, err)
	}

	return &udpBridge{
		conn:      conn,
		onInbound: onInbound,
	}, nil
}

func (u *udpBridge) Serve() {
	defer u.conn.Close()

	buf := make([]byte, 1500)
	for {
		n, addr, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("UDP read error: %v", err)
			continue
		}

		u.rememberESP32(addr)

		if u.onInbound != nil {
			chunk := append([]byte(nil), buf[:n]...)
			u.onInbound(chunk)
		}
	}
}

func (u *udpBridge) SendChunk(payload []byte) error {
	u.mu.RLock()
	addr := u.esp32Addr
	u.mu.RUnlock()

	if addr == nil {
		return fmt.Errorf("ESP32 endpoint unknown")
	}

	if _, err := u.conn.WriteToUDP(payload, addr); err != nil {
		return fmt.Errorf("UDP write to ESP32 %s failed: %w", addr.String(), err)
	}
	return nil
}

func (u *udpBridge) SendPCMU(payload []byte, frameBytes int, frameDelay time.Duration) error {
	if frameBytes <= 0 {
		frameBytes = 160
	}

	for offset := 0; offset < len(payload); offset += frameBytes {
		end := offset + frameBytes
		if end > len(payload) {
			end = len(payload)
		}

		if err := u.SendChunk(payload[offset:end]); err != nil {
			return err
		}

		if frameDelay > 0 && end < len(payload) {
			time.Sleep(frameDelay)
		}
	}

	return nil
}

func (u *udpBridge) ESP32Endpoint() string {
	u.mu.RLock()
	defer u.mu.RUnlock()

	if u.esp32Addr == nil {
		return ""
	}
	return u.esp32Addr.String()
}

func (u *udpBridge) rememberESP32(addr *net.UDPAddr) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.esp32Addr == nil || u.esp32Addr.String() != addr.String() {
		log.Printf("ESP32 UDP endpoint discovered: %s", addr.String())
	}
	u.esp32Addr = addr
}

package main

import "testing"

func TestIsUDPHeartbeat(t *testing.T) {
	if !isUDPHeartbeat(udpHeartbeatPayload) {
		t.Fatalf("expected canonical heartbeat payload to be detected")
	}

	if isUDPHeartbeat([]byte{0xFF, 0xFF}) {
		t.Fatalf("short all-0xFF payload must not be treated as heartbeat")
	}

	audioLike := append([]byte(nil), udpHeartbeatPayload...)
	audioLike[len(audioLike)-1] = 0x7F
	if isUDPHeartbeat(audioLike) {
		t.Fatalf("non-heartbeat audio payload detected as heartbeat")
	}
}

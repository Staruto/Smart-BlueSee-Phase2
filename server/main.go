package main

import (
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

type config struct {
	httpAddr string
	udpAddr  string
	webDir   string
}

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	udpServerConn *net.UDPConn
	esp32Addr     *net.UDPAddr
	esp32AddrMu   sync.RWMutex
	localTrack    *webrtc.TrackLocalStaticSample
)

func main() {
	cfg := loadConfig()

	var err error
	udpAddr, err := net.ResolveUDPAddr("udp", cfg.udpAddr)
	if err != nil {
		log.Fatalf("resolve UDP addr %q: %v", cfg.udpAddr, err)
	}

	udpServerConn, err = net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Fatalf("listen UDP on %q: %v", cfg.udpAddr, err)
	}
	defer udpServerConn.Close()
	log.Printf("UDP bridge listening on %s", cfg.udpAddr)

	localTrack, err = webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU},
		"audio",
		"pion",
	)
	if err != nil {
		log.Fatalf("create local track: %v", err)
	}

	go bridgeESP32ToBrowser()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", handleSignaling)
	if cfg.webDir != "" {
		log.Printf("Serving static files from %s", cfg.webDir)
		mux.Handle("/", http.FileServer(http.Dir(cfg.webDir)))
	}

	log.Printf("HTTP/WebSocket server listening on %s", cfg.httpAddr)
	if err := http.ListenAndServe(cfg.httpAddr, mux); err != nil {
		log.Fatal(err)
	}
}

func loadConfig() config {
	cfg := config{
		httpAddr: envOrDefault("WEBRTC_HTTP_ADDR", ":8080"),
		udpAddr:  envOrDefault("WEBRTC_UDP_ADDR", ":5000"),
		webDir:   envOrDefault("WEBRTC_WEB_DIR", "../web"),
	}

	flag.StringVar(&cfg.httpAddr, "http", cfg.httpAddr, "HTTP listen address")
	flag.StringVar(&cfg.udpAddr, "udp", cfg.udpAddr, "UDP listen address for ESP32 audio")
	flag.StringVar(&cfg.webDir, "web-dir", cfg.webDir, "Directory for static web assets; empty disables static serving")
	flag.Parse()

	return cfg
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func bridgeESP32ToBrowser() {
	buf := make([]byte, 1500)
	for {
		n, addr, err := udpServerConn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("UDP read error: %v", err)
			continue
		}

		esp32AddrMu.Lock()
		if esp32Addr == nil || esp32Addr.String() != addr.String() {
			log.Printf("ESP32 UDP endpoint discovered: %s", addr.String())
		}
		esp32Addr = addr
		esp32AddrMu.Unlock()

		duration := time.Duration(n) * time.Second / 8000
		if err := localTrack.WriteSample(media.Sample{
			Data:     append([]byte(nil), buf[:n]...),
			Duration: duration,
		}); err != nil {
			log.Printf("write sample to browser track: %v", err)
		}
	}
}

func handleSignaling(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed from %s: %v", r.RemoteAddr, err)
		return
	}
	defer c.Close()

	log.Printf("Browser signaling connected: %s", r.RemoteAddr)
	defer log.Printf("Browser signaling disconnected: %s", r.RemoteAddr)

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: 8000,
			Channels:  1,
		},
		PayloadType: 0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		log.Printf("register codec failed: %v", err)
		return
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
	peerConnection, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		log.Printf("create peer connection failed: %v", err)
		return
	}
	defer peerConnection.Close()

	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("PeerConnection state for %s: %s", r.RemoteAddr, state.String())
	})

	if _, err = peerConnection.AddTrack(localTrack); err != nil {
		log.Printf("attach local track failed: %v", err)
		return
	}

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		_ = receiver
		log.Printf("Receiving browser audio track from %s: %s", r.RemoteAddr, track.Codec().MimeType)
		for {
			rtpPacket, _, err := track.ReadRTP()
			if err != nil {
				log.Printf("browser RTP read ended for %s: %v", r.RemoteAddr, err)
				return
			}

			esp32AddrMu.RLock()
			addr := esp32Addr
			esp32AddrMu.RUnlock()

			if addr == nil {
				continue
			}

			if _, err := udpServerConn.WriteToUDP(rtpPacket.Payload, addr); err != nil {
				log.Printf("UDP write to ESP32 %s failed: %v", addr.String(), err)
			}
		}
	})

	for {
		_, message, err := c.ReadMessage()
		if err != nil {
			return
		}

		var signal map[string]any
		if err := json.Unmarshal(message, &signal); err != nil {
			log.Printf("invalid signaling message from %s: %v", r.RemoteAddr, err)
			continue
		}

		switch signal["type"] {
		case "offer":
			var offer webrtc.SessionDescription
			if err := json.Unmarshal(message, &offer); err != nil {
				log.Printf("decode offer failed: %v", err)
				continue
			}
			if err := peerConnection.SetRemoteDescription(offer); err != nil {
				log.Printf("set remote description failed: %v", err)
				continue
			}

			answer, err := peerConnection.CreateAnswer(nil)
			if err != nil {
				log.Printf("create answer failed: %v", err)
				continue
			}

			gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
			if err := peerConnection.SetLocalDescription(answer); err != nil {
				log.Printf("set local description failed: %v", err)
				continue
			}
			<-gatherComplete

			answerJSON, err := json.Marshal(peerConnection.LocalDescription())
			if err != nil {
				log.Printf("marshal answer failed: %v", err)
				continue
			}
			if err := c.WriteMessage(websocket.TextMessage, answerJSON); err != nil {
				log.Printf("send answer failed: %v", err)
				return
			}
		case "candidate":
			candidateValue, ok := signal["candidate"]
			if !ok {
				log.Printf("candidate message missing payload from %s", r.RemoteAddr)
				continue
			}

			candidateBytes, err := json.Marshal(candidateValue)
			if err != nil {
				log.Printf("marshal candidate failed: %v", err)
				continue
			}

			var candidate webrtc.ICECandidateInit
			if err := json.Unmarshal(candidateBytes, &candidate); err != nil {
				log.Printf("decode candidate failed: %v", err)
				continue
			}
			if err := peerConnection.AddICECandidate(candidate); err != nil {
				log.Printf("add ICE candidate failed: %v", err)
			}
		default:
			log.Printf("Ignoring unsupported signaling message type from %s: %v", r.RemoteAddr, signal["type"])
		}
	}
}

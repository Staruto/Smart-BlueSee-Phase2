package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

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
	// 1. 初始化 UDP 监听 (对接 ESP32)
	udpAddr, err := net.ResolveUDPAddr("udp", "0.0.0.0:5000")
	if err != nil {
		log.Fatal(err)
	}
	udpServerConn, err = net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Fatal(err)
	}
	defer udpServerConn.Close()
	log.Println("UDP 服务器正在监听 0.0.0.0:5000 用于对接 ESP32")

	// 准备 WebRTC 下行音轨 (Go -> Browser)，指定使用 PCMU (8000Hz)
	localTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU}, "audio", "pion")
	if err != nil {
		log.Fatal(err)
	}

	// 启动 UDP 接收循环 (ESP32 -> UDP -> Go -> WebRTC)
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := udpServerConn.ReadFromUDP(buf)
			if err != nil {
				continue
			}

			// 记录/更新当前发件 ESP32 通信地址，以便下发浏览器音频
			esp32AddrMu.Lock()
			if esp32Addr == nil || esp32Addr.String() != addr.String() {
				log.Printf("发现 ESP32 UDP 连接: %s", addr.String())
			}
			esp32Addr = addr
			esp32AddrMu.Unlock()

			// 将 ESP32 传来的 PCMU 取出放入 WebRTC Track
			// PCMU 单通道 8000Hz 对应 1 字节 = 1 采样 (1/8000 秒)
			duration := time.Duration(n) * time.Second / 8000
			_ = localTrack.WriteSample(media.Sample{
				Data:     buf[:n],
				Duration: duration,
			})
		}
	}()

	// 2. 提供静态服务与 WebSocket信令
	fs := http.FileServer(http.Dir("../web"))
	http.Handle("/", fs)
	http.HandleFunc("/ws", handleSignaling)

	log.Println("信令/Web 服务器已启动，请访问 http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}

func handleSignaling(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1},
		PayloadType:        0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		log.Println("注册编解码器失败:", err)
		return
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
	peerConnection, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		return
	}
	defer peerConnection.Close()

	// 提前将用来承载 ESP32 话音的 Track 挂载给 WebRTC
	if _, err = peerConnection.AddTrack(localTrack); err != nil {
		return
	}

	// 接收来自浏览器的音轨 (Browser -> Go -> ESP32)
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("接收到浏览器音频轨: %s", track.Codec().MimeType)
		for {
			rtpPacket, _, err := track.ReadRTP()
			if err != nil {
				return
			}

			// rtpPacket.Payload 是去除RTP头后的纯 PCMU 数据
			// 提取出来丢向 UDP 发送给 ESP32
			esp32AddrMu.RLock()
			addr := esp32Addr
			esp32AddrMu.RUnlock()

			if addr != nil && udpServerConn != nil {
				udpServerConn.WriteToUDP(rtpPacket.Payload, addr)
			}
		}
	})

	for {
		_, message, err := c.ReadMessage()
		if err != nil {
			break
		}
		var signal map[string]interface{}
		if err := json.Unmarshal(message, &signal); err != nil {
			continue
		}

		if signal["type"] == "offer" {
			offer := webrtc.SessionDescription{}
			json.Unmarshal(message, &offer)
			_ = peerConnection.SetRemoteDescription(offer)
			answer, _ := peerConnection.CreateAnswer(nil)
			gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
			_ = peerConnection.SetLocalDescription(answer)
			<-gatherComplete
			answerJSON, _ := json.Marshal(peerConnection.LocalDescription())
			c.WriteMessage(websocket.TextMessage, answerJSON)
		} else if signal["type"] == "candidate" {
			candidateData := signal["candidate"].(map[string]interface{})
			candidateString, _ := json.Marshal(candidateData)
			candidate := webrtc.ICECandidateInit{}
			json.Unmarshal(candidateString, &candidate)
			peerConnection.AddICECandidate(candidate)
		}
	}
}

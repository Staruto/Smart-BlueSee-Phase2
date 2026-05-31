package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	// 1. 提供静态网页文件服务
	fs := http.FileServer(http.Dir("../web"))
	http.Handle("/", fs)

	// 2. WebSocket 信令通道
	http.HandleFunc("/ws", handleSignaling)

	log.Println("服务器已启动，请访问 http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}

func handleSignaling(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("upgrade:", err)
		return
	}
	defer c.Close()

	// 初始化 WebRTC PeerConnection
	// 这里后续我们会强制指定只支持 PCMU (G.711) 编码，以便于 ESP32 处理
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1, SDPFmtpLine: "", RTCPFeedback: nil},
		PayloadType:        0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		log.Println("Failed to register codec:", err)
		return
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
	
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	peerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		log.Println("PeerConnection error:", err)
		return
	}
	defer peerConnection.Close()

	// 接收来自浏览器的音轨 (Browser -> ESP32 的上行流)
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("收到新的音轨，类型: %s, 载荷类型: %d", track.Codec().MimeType, track.PayloadType())
		// TODO: 第 2 阶段，这里会将 track.ReadRTP 读到的数据，通过 UDP 发给 ESP32
		
		// 临时回音测试：读取数据并丢弃，打印日志
		for {
			_, _, err := track.ReadRTP()
			if err != nil {
				log.Println("读取远端 RTP 失败:", err)
				return
			}
		}
	})

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.Printf("WebRTC ICE 状态改变: %s", connectionState.String())
	})

	// TODO: 第 2 阶段，这里需要创建一个 LocalTrack(PCMU)，送给 PeerConnection
	// 这样才能把从 ESP32 读取到的 UDP 音频发给浏览器

	// 信令循环：接收并处理浏览器的 Offer 和 ICE
	for {
		_, message, err := c.ReadMessage()
		if err != nil {
			log.Println("read:", err)
			break
		}

		var signal map[string]interface{}
		if err := json.Unmarshal(message, &signal); err != nil {
			log.Println("unmarshal:", err)
			continue
		}

		if signal["type"] == "offer" {
			offer := webrtc.SessionDescription{}
			json.Unmarshal(message, &offer)
			
			if err := peerConnection.SetRemoteDescription(offer); err != nil {
				log.Println("SetRemoteDescription error:", err)
				break
			}

			// 创建 Answer
			answer, err := peerConnection.CreateAnswer(nil)
			if err != nil {
				log.Println("CreateAnswer error:", err)
				break
			}
			
			// 收集 ICE Candidate 后发送 Answer
			gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
			if err := peerConnection.SetLocalDescription(answer); err != nil {
				log.Println("SetLocalDescription error:", err)
				break
			}
			<-gatherComplete

			answerJSON, _ := json.Marshal(peerConnection.LocalDescription())
			if err := c.WriteMessage(websocket.TextMessage, answerJSON); err != nil {
				log.Println("write answer error:", err)
				break
			}
		} else if signal["type"] == "candidate" {
			// 处理 ICE
			candidateData := signal["candidate"].(map[string]interface{})
			candidateString, _ := json.Marshal(candidateData)
			candidate := webrtc.ICECandidateInit{}
			json.Unmarshal(candidateString, &candidate)
			if err := peerConnection.AddICECandidate(candidate); err != nil {
				log.Println("AddICECandidate error:", err)
			}
		}
	}
}

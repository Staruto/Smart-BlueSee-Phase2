package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

type webRTCBridge struct {
	upgrader websocket.Upgrader
	udp      *udpBridge
	track    *webrtc.TrackLocalStaticSample
}

func newWebRTCBridge(udp *udpBridge) (*webRTCBridge, error) {
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU},
		"audio",
		"pion",
	)
	if err != nil {
		return nil, err
	}

	return &webRTCBridge{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		udp:   udp,
		track: track,
	}, nil
}

func (w *webRTCBridge) WriteInboundPCMU(payload []byte) {
	duration := time.Duration(len(payload)) * time.Second / 8000
	if err := w.track.WriteSample(media.Sample{
		Data:     append([]byte(nil), payload...),
		Duration: duration,
	}); err != nil {
		log.Printf("write sample to browser track: %v", err)
	}
}

func (w *webRTCBridge) HandleSignaling(rw http.ResponseWriter, req *http.Request) {
	conn, err := w.upgrader.Upgrade(rw, req, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed from %s: %v", req.RemoteAddr, err)
		return
	}
	defer conn.Close()

	log.Printf("Browser signaling connected: %s", req.RemoteAddr)
	defer log.Printf("Browser signaling disconnected: %s", req.RemoteAddr)

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
		log.Printf("PeerConnection state for %s: %s", req.RemoteAddr, state.String())
	})

	if _, err = peerConnection.AddTrack(w.track); err != nil {
		log.Printf("attach local track failed: %v", err)
		return
	}

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		_ = receiver
		log.Printf("Receiving browser audio track from %s: %s", req.RemoteAddr, track.Codec().MimeType)
		for {
			rtpPacket, _, err := track.ReadRTP()
			if err != nil {
				log.Printf("browser RTP read ended for %s: %v", req.RemoteAddr, err)
				return
			}

			if err := w.udp.SendChunk(rtpPacket.Payload); err != nil {
				log.Printf("browser audio forward failed: %v", err)
			}
		}
	})

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var signal map[string]any
		if err := json.Unmarshal(message, &signal); err != nil {
			log.Printf("invalid signaling message from %s: %v", req.RemoteAddr, err)
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
			if err := conn.WriteMessage(websocket.TextMessage, answerJSON); err != nil {
				log.Printf("send answer failed: %v", err)
				return
			}
		case "candidate":
			candidateValue, ok := signal["candidate"]
			if !ok {
				log.Printf("candidate message missing payload from %s", req.RemoteAddr)
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
			log.Printf("Ignoring unsupported signaling message type from %s: %v", req.RemoteAddr, signal["type"])
		}
	}
}

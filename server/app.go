package main

import (
	"log"
	"net/http"
)

type serverApp struct {
	cfg    config
	udp    *udpBridge
	webrtc *webRTCBridge
	voice  *voiceAgentService
}

func newServerApp(cfg config) (*serverApp, error) {
	app := &serverApp{cfg: cfg}

	udp, err := newUDPBridge(cfg.udpAddr, app.handleInboundAudio)
	if err != nil {
		return nil, err
	}
	app.udp = udp

	if cfg.enableWebRTC {
		webrtcBridge, err := newWebRTCBridge(app.udp)
		if err != nil {
			return nil, err
		}
		app.webrtc = webrtcBridge
	}

	if cfg.enableVoiceAgent {
		asrClient, err := newASRClient(cfg)
		if err != nil {
			return nil, err
		}
		agentClient, err := newAgentClient(cfg)
		if err != nil {
			return nil, err
		}
		ttsClient, err := newTTSClient(cfg)
		if err != nil {
			return nil, err
		}

		app.voice = newVoiceAgentService(cfg, asrClient, agentClient, ttsClient, func(audio []byte) error {
			return app.udp.SendPCMU(audio, cfg.ttsFrameBytes, cfg.ttsFrameDelay)
		})
	}

	return app, nil
}

func (a *serverApp) start() error {
	log.Printf("UDP bridge listening on %s", a.cfg.udpAddr)
	if a.cfg.enableWebRTC {
		log.Printf("WebRTC bridge enabled")
	}
	if a.cfg.enableVoiceAgent {
		log.Printf(
			"Voice agent enabled (ASR=%s LLM=%s TTS=%s session=%s)",
			a.cfg.asrBackend,
			a.cfg.llmBackend,
			a.cfg.ttsBackend,
			a.cfg.sessionID,
		)
	}

	go a.udp.Serve()
	return nil
}

func (a *serverApp) handleInboundAudio(payload []byte) {
	if a.webrtc != nil {
		a.webrtc.WriteInboundPCMU(payload)
	}
	if a.voice != nil {
		a.voice.IngestAudio(payload)
	}
}

func (a *serverApp) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", a.handleHealthz)

	if a.webrtc != nil {
		mux.HandleFunc("/ws", a.webrtc.HandleSignaling)
	}

	if a.voice != nil {
		mux.HandleFunc("/api/voice/status", a.handleVoiceStatus)
		mux.HandleFunc("/api/voice/commit", a.handleVoiceCommit)
		mux.HandleFunc("/api/voice/text-turn", a.handleVoiceTextTurn)
		mux.HandleFunc("/api/voice/reset", a.handleVoiceReset)
	}

	if a.cfg.webDir != "" {
		log.Printf("Serving static files from %s", a.cfg.webDir)
		mux.Handle("/", http.FileServer(http.Dir(a.cfg.webDir)))
	}
}

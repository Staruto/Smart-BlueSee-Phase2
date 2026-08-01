package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

type serverApp struct {
	cfg    config
	udp    *udpBridge
	webrtc *webRTCBridge
	voice  *voiceAgentService
	rag    *ragStore
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
		if cfg.ragEnable {
			rag, err := loadRAGStore(cfg.ragDir)
			if err != nil {
				return nil, err
			}
			app.rag = rag
		}

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

		app.voice = newVoiceAgentService(cfg, asrClient, agentClient, ttsClient, app.rag, func(audio []byte) error {
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
		if a.cfg.ragEnable {
			files, sections := a.rag.Stats()
			log.Printf(
				"Voice RAG enabled (dir=%s files=%d sections=%d top_k=%d max_context_chars=%d min_score=%.3f)",
				a.cfg.ragDir,
				files,
				sections,
				a.cfg.ragTopK,
				a.cfg.ragMaxContextChars,
				a.cfg.ragMinScore,
			)
		} else {
			log.Printf("Voice RAG disabled")
		}
		if a.cfg.autoCommit {
			log.Printf(
				"Voice auto-commit enabled (mode=%s idle=%s min_bytes=%d min_audio=%s min_rms_db=%.1f poll=%s)",
				a.cfg.autoCommitMode,
				a.cfg.autoCommitIdle,
				a.cfg.autoCommitMinBytes,
				a.cfg.autoCommitMinAudio,
				a.cfg.autoCommitMinRMSDB,
				a.cfg.autoCommitPoll,
			)
			go a.runVoiceAutoCommitLoop()
		}
	}

	go a.udp.Serve()
	return nil
}

func (a *serverApp) runVoiceAutoCommitLoop() {
	poll := a.cfg.autoCommitPoll
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for range ticker.C {
		if a.voice == nil || !a.voice.ShouldAutoCommit(time.Now()) {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		turn, err := a.runVoiceAutoCommitTurn(ctx)
		cancel()
		if err != nil {
			log.Printf("Voice auto-commit failed: %v", err)
			continue
		}

		timing := turn.Timing
		if timing == nil {
			timing = &voiceTurnTiming{}
		}
		log.Printf(
			"Voice auto-commit completed: mode=%s turn=%d input_bytes=%d output_bytes=%d total_ms=%d asr_ms=%d llm_ms=%d tts_ms=%d playback_ms=%d input=%q reply=%q",
			a.cfg.autoCommitMode,
			turn.TurnID,
			turn.InputAudioBytes,
			turn.OutputAudioBytes,
			timing.TotalMs,
			timing.ASRTotalMs,
			timing.LLMTotalMs,
			timing.TTSTotalMs,
			timing.PlaybackSendMs,
			turn.InputText,
			turn.ReplyText,
		)
	}
}

func (a *serverApp) runVoiceAutoCommitTurn(ctx context.Context) (voiceTurnResult, error) {
	switch strings.ToLower(strings.TrimSpace(a.cfg.autoCommitMode)) {
	case "", "agent":
		return a.voice.CommitBufferedAudioAuto(ctx, true)
	case "loopback", "raw-loopback", "raw":
		return a.voice.LoopbackBufferedAudio(ctx, "auto_loopback")
	default:
		return voiceTurnResult{}, fmt.Errorf("unsupported VOICE_AGENT_AUTO_COMMIT_MODE %q", a.cfg.autoCommitMode)
	}
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
		mux.HandleFunc("/api/voice/loopback", a.handleVoiceLoopback)
		mux.HandleFunc("/api/voice/audio-turn", a.handleVoiceAudioTurn)
		mux.HandleFunc("/api/voice/text-turn", a.handleVoiceTextTurn)
		mux.HandleFunc("/api/voice/reset", a.handleVoiceReset)
	}

	if a.cfg.webDir != "" {
		log.Printf("Serving static files from %s", a.cfg.webDir)
		mux.Handle("/", http.FileServer(http.Dir(a.cfg.webDir)))
	}
}

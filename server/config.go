package main

import (
	"flag"
	"os"
	"strconv"
	"time"
)

type config struct {
	httpAddr         string
	udpAddr          string
	webDir           string
	enableWebRTC     bool
	enableVoiceAgent bool
	sessionID        string
	systemPrompt     string
	asrBackend       string
	asrEndpoint      string
	llmBackend       string
	llmEndpoint      string
	ttsBackend       string
	ttsEndpoint      string
	mockASRText      string
	ttsFrameBytes    int
	ttsFrameDelay    time.Duration
}

func loadConfig() config {
	cfg := config{
		httpAddr:         envOrDefault("WEBRTC_HTTP_ADDR", ":8080"),
		udpAddr:          envOrDefault("WEBRTC_UDP_ADDR", ":5000"),
		webDir:           envOrDefault("WEBRTC_WEB_DIR", "../web"),
		enableWebRTC:     envBoolOrDefault("WEBRTC_ENABLE", true),
		enableVoiceAgent: envBoolOrDefault("VOICE_AGENT_ENABLE", true),
		sessionID:        envOrDefault("VOICE_AGENT_SESSION_ID", "esp32-default"),
		systemPrompt:     envOrDefault("VOICE_AGENT_SYSTEM_PROMPT", "You are a concise voice assistant. Reply with short, speakable sentences."),
		asrBackend:       envOrDefault("VOICE_AGENT_ASR_BACKEND", "mock"),
		asrEndpoint:      envOrDefault("VOICE_AGENT_ASR_ENDPOINT", "http://127.0.0.1:8091/transcribe"),
		llmBackend:       envOrDefault("VOICE_AGENT_LLM_BACKEND", "mock"),
		llmEndpoint:      envOrDefault("VOICE_AGENT_LLM_ENDPOINT", "http://127.0.0.1:8092/respond"),
		ttsBackend:       envOrDefault("VOICE_AGENT_TTS_BACKEND", "mock"),
		ttsEndpoint:      envOrDefault("VOICE_AGENT_TTS_ENDPOINT", "http://127.0.0.1:8093/synthesize"),
		mockASRText:      envOrDefault("VOICE_AGENT_MOCK_ASR_TEXT", ""),
		ttsFrameBytes:    envIntOrDefault("VOICE_AGENT_TTS_FRAME_BYTES", 160),
		ttsFrameDelay:    envDurationOrDefault("VOICE_AGENT_TTS_FRAME_DELAY", 20*time.Millisecond),
	}

	flag.StringVar(&cfg.httpAddr, "http", cfg.httpAddr, "HTTP listen address")
	flag.StringVar(&cfg.udpAddr, "udp", cfg.udpAddr, "UDP listen address for ESP32 audio")
	flag.StringVar(&cfg.webDir, "web-dir", cfg.webDir, "Directory for static web assets; empty disables static serving")
	flag.BoolVar(&cfg.enableWebRTC, "enable-webrtc", cfg.enableWebRTC, "Enable browser WebRTC signaling and media bridge")
	flag.BoolVar(&cfg.enableVoiceAgent, "enable-voice-agent", cfg.enableVoiceAgent, "Enable buffered ASR -> LLM -> TTS pipeline")
	flag.StringVar(&cfg.sessionID, "session-id", cfg.sessionID, "Voice agent session identifier")
	flag.StringVar(&cfg.systemPrompt, "system-prompt", cfg.systemPrompt, "System prompt for the LLM backend")
	flag.StringVar(&cfg.asrBackend, "asr-backend", cfg.asrBackend, "ASR backend: mock or http")
	flag.StringVar(&cfg.asrEndpoint, "asr-endpoint", cfg.asrEndpoint, "ASR HTTP endpoint")
	flag.StringVar(&cfg.llmBackend, "llm-backend", cfg.llmBackend, "LLM backend: mock or http")
	flag.StringVar(&cfg.llmEndpoint, "llm-endpoint", cfg.llmEndpoint, "LLM HTTP endpoint")
	flag.StringVar(&cfg.ttsBackend, "tts-backend", cfg.ttsBackend, "TTS backend: mock or http")
	flag.StringVar(&cfg.ttsEndpoint, "tts-endpoint", cfg.ttsEndpoint, "TTS HTTP endpoint")
	flag.StringVar(&cfg.mockASRText, "mock-asr-text", cfg.mockASRText, "Static text returned by the mock ASR backend")
	flag.IntVar(&cfg.ttsFrameBytes, "tts-frame-bytes", cfg.ttsFrameBytes, "Chunk size used when streaming synthesized audio back to ESP32")
	flag.DurationVar(&cfg.ttsFrameDelay, "tts-frame-delay", cfg.ttsFrameDelay, "Delay between synthesized audio chunks sent to ESP32")
	flag.Parse()

	return cfg
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envBoolOrDefault(name string, fallback bool) bool {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}

	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envIntOrDefault(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDurationOrDefault(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}

	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

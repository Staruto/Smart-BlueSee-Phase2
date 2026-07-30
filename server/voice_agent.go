package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	audioEncodingG711ULaw = "g711_ulaw"
	audioSampleRateHz     = 8000
	audioChannels         = 1
	maxHistoryMessages    = 12
	disableAutoCommitRMS  = -100
)

const blueSeeSystemPrompt = `You are BlueSee, the mascot and AI assistant for the Faculty of Science and Engineering at University of Nottingham Ningbo China.

You serve FoSE students and staff. Answer general questions normally, and answer university-specific questions using retrieved campus context when it is available.

Style:
- Keep replies concise, friendly, and suitable for spoken TTS.
- Prefer direct next steps over long explanations.
- If the user asks in Chinese, answer in Chinese. Otherwise answer in the user's language.

Grounding rules:
- Use retrieved campus context over model memory for University of Nottingham Ningbo China or FoSE facts.
- Do not invent office names, deadlines, phone numbers, policies, room locations, programme rules, or staff responsibilities.
- If the retrieved context is missing or insufficient for a university-specific question, say that the current knowledge base does not contain enough information and suggest checking the official UNNC website, The Hub, or the relevant FoSE/admin office.
- For time-sensitive facts, say when the answer is not live-verified unless the retrieved context provides the detail.
- For urgent danger, medical emergencies, or self-harm risk, advise contacting local emergency/professional support immediately.`

var errASREmptyText = errors.New("ASR returned empty text")

type audioFormat struct {
	Encoding     string `json:"encoding"`
	SampleRateHz int    `json:"sample_rate_hz"`
	Channels     int    `json:"channels"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type toolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type toolCall struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type agentTraceStep struct {
	Step     int       `json:"step"`
	Type     string    `json:"type"`
	Summary  string    `json:"summary"`
	ToolCall *toolCall `json:"tool_call,omitempty"`
}

type asrRequest struct {
	SessionID   string      `json:"session_id"`
	AudioFormat audioFormat `json:"audio_format"`
	AudioBase64 string      `json:"audio_base64"`
}

type asrResult struct {
	Text  string         `json:"text"`
	Final bool           `json:"final"`
	Meta  map[string]any `json:"meta,omitempty"`
}

type agentRequest struct {
	SessionID       string           `json:"session_id"`
	SystemPrompt    string           `json:"system_prompt,omitempty"`
	Messages        []chatMessage    `json:"messages"`
	Tools           []toolDefinition `json:"tools,omitempty"`
	EnableToolCalls bool             `json:"enable_tool_calls"`
	MaxSteps        int              `json:"max_steps"`
}

type agentResult struct {
	Text       string           `json:"text"`
	StopReason string           `json:"stop_reason,omitempty"`
	Trace      []agentTraceStep `json:"trace,omitempty"`
}

type ttsRequest struct {
	SessionID   string      `json:"session_id"`
	Text        string      `json:"text"`
	AudioFormat audioFormat `json:"audio_format"`
}

type ttsResult struct {
	Audio []byte         `json:"-"`
	Meta  map[string]any `json:"meta,omitempty"`
}

type voiceTurnTiming struct {
	Trigger          string `json:"trigger"`
	TotalMs          int    `json:"total_ms"`
	ASRTotalMs       int    `json:"asr_total_ms,omitempty"`
	ASRBackendMs     int    `json:"asr_backend_ms,omitempty"`
	LLMTotalMs       int    `json:"llm_total_ms,omitempty"`
	TTSTotalMs       int    `json:"tts_total_ms,omitempty"`
	TTSBackendMs     int    `json:"tts_backend_ms,omitempty"`
	PlaybackSendMs   int    `json:"playback_send_ms,omitempty"`
	AutoCommitIdleMs int    `json:"auto_commit_idle_ms,omitempty"`
	BufferAgeMs      int    `json:"buffer_age_ms,omitempty"`
	InputAudioMs     int    `json:"input_audio_ms,omitempty"`
}

type voiceTurnResult struct {
	TurnID           int              `json:"turn_id"`
	CreatedAt        time.Time        `json:"created_at"`
	Source           string           `json:"source"`
	InputText        string           `json:"input_text"`
	ReplyText        string           `json:"reply_text"`
	InputAudioBytes  int              `json:"input_audio_bytes"`
	OutputAudioBytes int              `json:"output_audio_bytes"`
	Trace            []agentTraceStep `json:"trace,omitempty"`
	Timing           *voiceTurnTiming `json:"timing,omitempty"`
}

type voiceStatus struct {
	SessionID          string            `json:"session_id"`
	BufferedAudioBytes int               `json:"buffered_audio_bytes"`
	Processing         bool              `json:"processing"`
	HistoryMessages    int               `json:"history_messages"`
	ESP32Endpoint      string            `json:"esp32_endpoint,omitempty"`
	Backends           map[string]string `json:"backends"`
	RAGEnabled         bool              `json:"rag_enabled"`
	RAGFiles           int               `json:"rag_files,omitempty"`
	RAGSections        int               `json:"rag_sections,omitempty"`
	AutoCommit         bool              `json:"auto_commit"`
	AutoCommitMode     string            `json:"auto_commit_mode,omitempty"`
	AutoCommitIdleMs   int               `json:"auto_commit_idle_ms,omitempty"`
	AutoCommitMinBytes int               `json:"auto_commit_min_bytes,omitempty"`
	AutoCommitMinAudio int               `json:"auto_commit_min_audio_ms,omitempty"`
	AutoCommitMinRMSDB float64           `json:"auto_commit_min_rms_db,omitempty"`
	LastTurn           *voiceTurnResult  `json:"last_turn,omitempty"`
}

type ASRClient interface {
	Transcribe(ctx context.Context, req asrRequest) (asrResult, error)
}

type AgentClient interface {
	Run(ctx context.Context, req agentRequest) (agentResult, error)
}

type TTSClient interface {
	Synthesize(ctx context.Context, req ttsRequest) (ttsResult, error)
}

type voiceAgentService struct {
	cfg       config
	asr       ASRClient
	agent     AgentClient
	tts       TTSClient
	rag       *ragStore
	playAudio func([]byte) error

	mu          sync.Mutex
	buffer      []byte
	history     []chatMessage
	lastTurn    *voiceTurnResult
	turnID      int
	busy        bool
	lastAudioAt time.Time
}

func newVoiceAgentService(cfg config, asr ASRClient, agent AgentClient, tts TTSClient, rag *ragStore, playAudio func([]byte) error) *voiceAgentService {
	return &voiceAgentService{
		cfg:       cfg,
		asr:       asr,
		agent:     agent,
		tts:       tts,
		rag:       rag,
		playAudio: playAudio,
	}
}

func (v *voiceAgentService) IngestAudio(payload []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.buffer = append(v.buffer, payload...)
	v.lastAudioAt = time.Now()
}

func (v *voiceAgentService) Status(esp32Endpoint string) voiceStatus {
	v.mu.Lock()
	defer v.mu.Unlock()

	status := voiceStatus{
		SessionID:          v.cfg.sessionID,
		BufferedAudioBytes: len(v.buffer),
		Processing:         v.busy,
		HistoryMessages:    len(v.history),
		ESP32Endpoint:      esp32Endpoint,
		Backends: map[string]string{
			"asr": v.cfg.asrBackend,
			"llm": v.cfg.llmBackend,
			"tts": v.cfg.ttsBackend,
		},
		RAGEnabled:         v.cfg.ragEnable && v.rag != nil,
		AutoCommit:         v.cfg.autoCommit,
		AutoCommitMode:     v.cfg.autoCommitMode,
		AutoCommitIdleMs:   int(v.cfg.autoCommitIdle / time.Millisecond),
		AutoCommitMinBytes: v.cfg.autoCommitMinBytes,
		AutoCommitMinAudio: int(v.cfg.autoCommitMinAudio / time.Millisecond),
		AutoCommitMinRMSDB: v.cfg.autoCommitMinRMSDB,
	}
	if status.RAGEnabled {
		status.RAGFiles, status.RAGSections = v.rag.Stats()
	}
	if v.lastTurn != nil {
		turnCopy := *v.lastTurn
		status.LastTurn = &turnCopy
	}
	return status
}

func (v *voiceAgentService) Reset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.buffer = nil
	v.history = nil
	v.lastTurn = nil
	v.busy = false
	v.lastAudioAt = time.Time{}
}

func (v *voiceAgentService) ShouldAutoCommit(now time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.busy || len(v.buffer) < v.cfg.autoCommitMinBytes || v.lastAudioAt.IsZero() {
		return false
	}
	if now.Sub(v.lastAudioAt) < v.cfg.autoCommitIdle {
		return false
	}

	if v.cfg.autoCommitMinAudio > 0 && pcmuDuration(v.buffer) < v.cfg.autoCommitMinAudio {
		v.discardBufferedAudioLocked()
		return false
	}

	if autoCommitRMSGateEnabled(v.cfg.autoCommitMinRMSDB) && pcmuRMSDBFS(v.buffer) < v.cfg.autoCommitMinRMSDB {
		v.discardBufferedAudioLocked()
		return false
	}

	return true
}

func (v *voiceAgentService) CommitBufferedAudio(ctx context.Context, speak bool) (voiceTurnResult, error) {
	return v.commitBufferedAudio(ctx, speak, "manual_commit")
}

func (v *voiceAgentService) CommitBufferedAudioAuto(ctx context.Context, speak bool) (voiceTurnResult, error) {
	return v.commitBufferedAudio(ctx, speak, "auto_commit")
}

func (v *voiceAgentService) LoopbackBufferedAudio(ctx context.Context, trigger string) (voiceTurnResult, error) {
	if trigger == "" {
		trigger = "loopback"
	}

	started := time.Now()
	snapshot, bufferAge, err := v.beginBufferedTurn()
	if err != nil {
		return voiceTurnResult{}, err
	}

	timing := &voiceTurnTiming{
		Trigger:          trigger,
		BufferAgeMs:      durationMillis(bufferAge),
		AutoCommitIdleMs: autoCommitIdleMs(trigger, v.cfg.autoCommitIdle),
		InputAudioMs:     durationMillis(pcmuDuration(snapshot)),
	}

	if v.playAudio == nil {
		v.restoreBufferedAudio(snapshot)
		return voiceTurnResult{}, fmt.Errorf("audio playback is not configured")
	}

	playbackStarted := time.Now()
	if err := v.playAudio(snapshot); err != nil {
		v.restoreBufferedAudio(snapshot)
		return voiceTurnResult{}, err
	}
	timing.PlaybackSendMs = elapsedMillis(playbackStarted)
	timing.TotalMs = elapsedMillis(started)

	turn := voiceTurnResult{
		Source:           "loopback",
		InputAudioBytes:  len(snapshot),
		OutputAudioBytes: len(snapshot),
		Timing:           timing,
	}

	v.finishTurn(&turn)
	return turn, nil
}

func (v *voiceAgentService) commitBufferedAudio(ctx context.Context, speak bool, trigger string) (voiceTurnResult, error) {
	started := time.Now()
	snapshot, bufferAge, err := v.beginBufferedTurn()
	if err != nil {
		return voiceTurnResult{}, err
	}

	timing := &voiceTurnTiming{
		Trigger:          trigger,
		BufferAgeMs:      durationMillis(bufferAge),
		AutoCommitIdleMs: autoCommitIdleMs(trigger, v.cfg.autoCommitIdle),
		InputAudioMs:     durationMillis(pcmuDuration(snapshot)),
	}
	turn, err := v.executeAudioTurn(ctx, "audio", snapshot, speak, timing)
	if err != nil {
		if errors.Is(err, errASREmptyText) {
			v.discardBufferedTurn()
		} else {
			v.restoreBufferedAudio(snapshot)
		}
		return voiceTurnResult{}, err
	}

	turn.Timing.TotalMs = elapsedMillis(started)
	v.finishTurn(&turn)
	return turn, nil
}

func (v *voiceAgentService) RunAudioTurn(ctx context.Context, audio []byte, speak bool) (voiceTurnResult, error) {
	if len(audio) == 0 {
		return voiceTurnResult{}, fmt.Errorf("audio is required")
	}

	if err := v.beginTextTurn(); err != nil {
		return voiceTurnResult{}, err
	}

	started := time.Now()
	timing := &voiceTurnTiming{Trigger: "audio_turn"}
	turn, err := v.executeAudioTurn(ctx, "audio", audio, speak, timing)
	if err != nil {
		v.failTurn()
		return voiceTurnResult{}, err
	}

	turn.Timing.TotalMs = elapsedMillis(started)
	v.finishTurn(&turn)
	return turn, nil
}

func (v *voiceAgentService) executeAudioTurn(ctx context.Context, source string, audio []byte, speak bool, timing *voiceTurnTiming) (voiceTurnResult, error) {
	req := asrRequest{
		SessionID: v.cfg.sessionID,
		AudioFormat: audioFormat{
			Encoding:     audioEncodingG711ULaw,
			SampleRateHz: audioSampleRateHz,
			Channels:     audioChannels,
		},
		AudioBase64: base64.StdEncoding.EncodeToString(audio),
	}

	asrStarted := time.Now()
	asrResult, err := v.asr.Transcribe(ctx, req)
	timing.ASRTotalMs = elapsedMillis(asrStarted)
	if err != nil {
		return voiceTurnResult{}, err
	}
	timing.ASRBackendMs = metaLatencyMillis(asrResult.Meta)

	text := strings.TrimSpace(asrResult.Text)
	if text == "" {
		return voiceTurnResult{}, errASREmptyText
	}

	return v.executeTurn(ctx, source, text, len(audio), speak, timing)
}

func (v *voiceAgentService) RunTextTurn(ctx context.Context, text string, speak bool) (voiceTurnResult, error) {
	if strings.TrimSpace(text) == "" {
		return voiceTurnResult{}, fmt.Errorf("text is required")
	}

	if err := v.beginTextTurn(); err != nil {
		return voiceTurnResult{}, err
	}

	started := time.Now()
	timing := &voiceTurnTiming{Trigger: "text_turn"}
	turn, err := v.executeTurn(ctx, "text", text, 0, speak, timing)
	if err != nil {
		v.failTurn()
		return voiceTurnResult{}, err
	}

	turn.Timing.TotalMs = elapsedMillis(started)
	v.finishTurn(&turn)
	return turn, nil
}

func (v *voiceAgentService) beginBufferedTurn() ([]byte, time.Duration, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.busy {
		return nil, 0, fmt.Errorf("voice agent already processing a turn")
	}
	if len(v.buffer) == 0 {
		return nil, 0, fmt.Errorf("no buffered audio available")
	}

	snapshot := append([]byte(nil), v.buffer...)
	bufferAge := time.Duration(0)
	if !v.lastAudioAt.IsZero() {
		bufferAge = time.Since(v.lastAudioAt)
	}
	v.buffer = nil
	v.busy = true
	return snapshot, bufferAge, nil
}

func (v *voiceAgentService) beginTextTurn() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.busy {
		return fmt.Errorf("voice agent already processing a turn")
	}

	v.busy = true
	return nil
}

func (v *voiceAgentService) restoreBufferedAudio(snapshot []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.buffer = append(snapshot, v.buffer...)
	v.busy = false
}

func (v *voiceAgentService) discardBufferedTurn() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.busy = false
	if len(v.buffer) == 0 {
		v.lastAudioAt = time.Time{}
	}
}

func (v *voiceAgentService) discardBufferedAudioLocked() {
	v.buffer = nil
	v.lastAudioAt = time.Time{}
}

func (v *voiceAgentService) failTurn() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.busy = false
}

func (v *voiceAgentService) finishTurn(turn *voiceTurnResult) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.busy = false
	v.turnID++
	turn.TurnID = v.turnID
	turn.CreatedAt = time.Now().UTC()
	v.lastTurn = &voiceTurnResult{
		TurnID:           turn.TurnID,
		CreatedAt:        turn.CreatedAt,
		Source:           turn.Source,
		InputText:        turn.InputText,
		ReplyText:        turn.ReplyText,
		InputAudioBytes:  turn.InputAudioBytes,
		OutputAudioBytes: turn.OutputAudioBytes,
		Trace:            append([]agentTraceStep(nil), turn.Trace...),
		Timing:           cloneVoiceTurnTiming(turn.Timing),
	}
}

func (v *voiceAgentService) executeTurn(ctx context.Context, source string, text string, inputAudioBytes int, speak bool, timing *voiceTurnTiming) (voiceTurnResult, error) {
	messages := v.buildMessages(text)
	ragResult := v.retrieveRAG(text)
	agentReq := agentRequest{
		SessionID:       v.cfg.sessionID,
		SystemPrompt:    v.buildSystemPrompt(ragResult),
		Messages:        messages,
		EnableToolCalls: false,
		MaxSteps:        1,
		Tools: []toolDefinition{
			{
				Name:        "future_web_search",
				Description: "Reserved placeholder for a future MCP-backed web search tool",
			},
		},
	}

	llmStarted := time.Now()
	agentResp, err := v.agent.Run(ctx, agentReq)
	timing.LLMTotalMs = elapsedMillis(llmStarted)
	if err != nil {
		return voiceTurnResult{}, err
	}

	replyText := strings.TrimSpace(agentResp.Text)
	if replyText == "" {
		return voiceTurnResult{}, fmt.Errorf("LLM returned empty reply")
	}

	ttsStarted := time.Now()
	synthResp, err := v.tts.Synthesize(ctx, ttsRequest{
		SessionID: v.cfg.sessionID,
		Text:      replyText,
		AudioFormat: audioFormat{
			Encoding:     audioEncodingG711ULaw,
			SampleRateHz: audioSampleRateHz,
			Channels:     audioChannels,
		},
	})
	timing.TTSTotalMs = elapsedMillis(ttsStarted)
	if err != nil {
		return voiceTurnResult{}, err
	}
	timing.TTSBackendMs = metaLatencyMillis(synthResp.Meta)

	if speak && len(synthResp.Audio) > 0 {
		playbackStarted := time.Now()
		if err := v.playAudio(synthResp.Audio); err != nil {
			return voiceTurnResult{}, err
		}
		timing.PlaybackSendMs = elapsedMillis(playbackStarted)
	}

	v.appendHistory(text, replyText)

	trace := append([]agentTraceStep(nil), ragTraceStep(ragResult)...)
	trace = append(trace, agentResp.Trace...)
	turn := voiceTurnResult{
		Source:           source,
		InputText:        text,
		ReplyText:        replyText,
		InputAudioBytes:  inputAudioBytes,
		OutputAudioBytes: len(synthResp.Audio),
		Trace:            trace,
		Timing:           timing,
	}
	return turn, nil
}

func (v *voiceAgentService) retrieveRAG(text string) ragResult {
	if !v.cfg.ragEnable || v.rag == nil {
		return ragResult{Route: ragRouteDisabled}
	}
	return v.rag.Retrieve(text, v.cfg.ragTopK, v.cfg.ragMaxContextChars, v.cfg.ragMinScore)
}

func (v *voiceAgentService) buildSystemPrompt(result ragResult) string {
	parts := []string{blueSeeSystemPrompt}
	if strings.TrimSpace(v.cfg.systemPrompt) != "" {
		parts = append(parts, "Additional operator instruction:\n"+strings.TrimSpace(v.cfg.systemPrompt))
	}
	if strings.TrimSpace(result.Context) != "" {
		parts = append(parts, "Retrieved campus context:\n"+result.Context)
	}
	return strings.Join(parts, "\n\n")
}

func ragTraceStep(result ragResult) []agentTraceStep {
	if result.Route == "" || result.Route == ragRouteDisabled {
		return nil
	}
	summary := fmt.Sprintf("RAG route=%s files=%d sections=%d context_chars=%d", result.Route, result.Files, result.SectionCount, result.ContextChars)
	if len(result.Sections) > 0 {
		summary += " sources=" + strings.Join(result.Sections, "; ")
	}
	return []agentTraceStep{{Step: 1, Type: "rag", Summary: summary}}
}

func (v *voiceAgentService) buildMessages(userText string) []chatMessage {
	v.mu.Lock()
	defer v.mu.Unlock()

	messages := make([]chatMessage, 0, len(v.history)+1)
	messages = append(messages, v.history...)
	messages = append(messages, chatMessage{Role: "user", Content: userText})
	return messages
}

func (v *voiceAgentService) appendHistory(userText string, replyText string) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.history = append(v.history,
		chatMessage{Role: "user", Content: userText},
		chatMessage{Role: "assistant", Content: replyText},
	)
	if len(v.history) > maxHistoryMessages {
		v.history = append([]chatMessage(nil), v.history[len(v.history)-maxHistoryMessages:]...)
	}
}

func newASRClient(cfg config) (ASRClient, error) {
	switch strings.ToLower(cfg.asrBackend) {
	case "mock":
		return mockASRClient{staticText: cfg.mockASRText}, nil
	case "http":
		return httpASRClient{
			endpoint: cfg.asrEndpoint,
			client:   &http.Client{Timeout: 30 * time.Second},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported ASR backend %q", cfg.asrBackend)
	}
}

func newAgentClient(cfg config) (AgentClient, error) {
	switch strings.ToLower(cfg.llmBackend) {
	case "mock":
		return mockAgentClient{}, nil
	case "http":
		return httpAgentClient{
			endpoint: cfg.llmEndpoint,
			client:   &http.Client{Timeout: 60 * time.Second},
		}, nil
	case "openai":
		return openAICompatibleAgentClient{
			endpoint:  cfg.llmEndpoint,
			model:     cfg.llmModel,
			maxTokens: cfg.llmMaxTokens,
			apiKey:    cfg.llmAPIKey,
			client:    &http.Client{Timeout: 90 * time.Second},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported LLM backend %q", cfg.llmBackend)
	}
}

func newTTSClient(cfg config) (TTSClient, error) {
	switch strings.ToLower(cfg.ttsBackend) {
	case "mock":
		return mockTTSClient{}, nil
	case "http":
		return httpTTSClient{
			endpoint: cfg.ttsEndpoint,
			client:   &http.Client{Timeout: 60 * time.Second},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported TTS backend %q", cfg.ttsBackend)
	}
}

type mockASRClient struct {
	staticText string
}

func (m mockASRClient) Transcribe(_ context.Context, req asrRequest) (asrResult, error) {
	if m.staticText != "" {
		return asrResult{Text: m.staticText, Final: true}, nil
	}
	audioBytes, err := base64.StdEncoding.DecodeString(req.AudioBase64)
	if err != nil {
		return asrResult{}, err
	}
	return asrResult{
		Text:  fmt.Sprintf("Received %d bytes of microphone audio.", len(audioBytes)),
		Final: true,
		Meta:  map[string]any{"backend": "mock"},
	}, nil
}

type mockAgentClient struct{}

func (mockAgentClient) Run(_ context.Context, req agentRequest) (agentResult, error) {
	userText := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			userText = req.Messages[i].Content
			break
		}
	}

	replyText := "I heard you."
	if userText != "" {
		replyText = "I heard: " + strings.TrimSpace(userText)
	}

	return agentResult{
		Text:       replyText,
		StopReason: "mock_complete",
		Trace: []agentTraceStep{
			{Step: 1, Type: "reasoning", Summary: "Mock LLM produced a direct answer without tool calls."},
		},
	}, nil
}

type mockTTSClient struct{}

func (mockTTSClient) Synthesize(_ context.Context, req ttsRequest) (ttsResult, error) {
	audio := synthesizeMockTone(req.Text)
	return ttsResult{
		Audio: audio,
		Meta:  map[string]any{"backend": "mock", "note": "tone only, not spoken speech"},
	}, nil
}

type httpASRClient struct {
	endpoint string
	client   *http.Client
}

func (h httpASRClient) Transcribe(ctx context.Context, req asrRequest) (asrResult, error) {
	var resp struct {
		Text       string         `json:"text"`
		Transcript string         `json:"transcript"`
		Final      bool           `json:"final"`
		Meta       map[string]any `json:"meta,omitempty"`
	}
	if err := postJSON(ctx, h.client, h.endpoint, req, &resp); err != nil {
		return asrResult{}, err
	}

	text := resp.Text
	if text == "" {
		text = resp.Transcript
	}
	return asrResult{Text: text, Final: resp.Final, Meta: resp.Meta}, nil
}

type httpAgentClient struct {
	endpoint string
	client   *http.Client
}

func (h httpAgentClient) Run(ctx context.Context, req agentRequest) (agentResult, error) {
	var resp struct {
		Text       string           `json:"text"`
		Content    string           `json:"content"`
		StopReason string           `json:"stop_reason"`
		Trace      []agentTraceStep `json:"trace,omitempty"`
	}
	if err := postJSON(ctx, h.client, h.endpoint, req, &resp); err != nil {
		return agentResult{}, err
	}

	text := resp.Text
	if text == "" {
		text = resp.Content
	}
	return agentResult{Text: text, StopReason: resp.StopReason, Trace: resp.Trace}, nil
}

type openAICompatibleAgentClient struct {
	endpoint  string
	model     string
	maxTokens int
	apiKey    string
	client    *http.Client
}

func (o openAICompatibleAgentClient) Run(ctx context.Context, req agentRequest) (agentResult, error) {
	messages := make([]chatMessage, 0, len(req.Messages)+1)
	if strings.TrimSpace(req.SystemPrompt) != "" {
		messages = append(messages, chatMessage{Role: "system", Content: req.SystemPrompt})
	}
	messages = append(messages, req.Messages...)

	payload := struct {
		Model       string        `json:"model"`
		Messages    []chatMessage `json:"messages"`
		Stream      bool          `json:"stream"`
		MaxTokens   int           `json:"max_tokens,omitempty"`
		Temperature float64       `json:"temperature,omitempty"`
	}{
		Model:       o.model,
		Messages:    messages,
		Stream:      false,
		MaxTokens:   o.maxTokens,
		Temperature: 0.3,
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type,omitempty"`
		} `json:"error,omitempty"`
	}
	headers := map[string]string{}
	if strings.TrimSpace(o.apiKey) != "" {
		headers["Authorization"] = "Bearer " + strings.TrimSpace(o.apiKey)
	}

	if err := postJSONWithHeaders(ctx, o.client, o.endpoint, payload, &resp, headers, o.apiKey); err != nil {
		return agentResult{}, err
	}
	if resp.Error != nil {
		return agentResult{}, fmt.Errorf("OpenAI-compatible LLM error: %s", redactSecret(resp.Error.Message, o.apiKey))
	}
	if len(resp.Choices) == 0 {
		return agentResult{}, fmt.Errorf("OpenAI-compatible LLM returned no choices")
	}

	choice := resp.Choices[0]
	text := strings.TrimSpace(choice.Message.Content)
	if text == "" && strings.TrimSpace(choice.Message.ReasoningContent) != "" {
		return agentResult{}, fmt.Errorf("OpenAI-compatible LLM returned reasoning_content but empty final content; disable reasoning or increase/budget tokens")
	}
	return agentResult{
		Text:       text,
		StopReason: choice.FinishReason,
		Trace: []agentTraceStep{
			{Step: 1, Type: "llm", Summary: "OpenAI-compatible chat completion returned a final answer."},
		},
	}, nil
}

type httpTTSClient struct {
	endpoint string
	client   *http.Client
}

func (h httpTTSClient) Synthesize(ctx context.Context, req ttsRequest) (ttsResult, error) {
	var resp struct {
		AudioBase64 string         `json:"audio_base64"`
		AudioB64    string         `json:"audio_b64"`
		Meta        map[string]any `json:"meta,omitempty"`
	}
	if err := postJSON(ctx, h.client, h.endpoint, req, &resp); err != nil {
		return ttsResult{}, err
	}

	audioBase64 := resp.AudioBase64
	if audioBase64 == "" {
		audioBase64 = resp.AudioB64
	}

	audio, err := base64.StdEncoding.DecodeString(audioBase64)
	if err != nil {
		return ttsResult{}, err
	}
	return ttsResult{Audio: audio, Meta: resp.Meta}, nil
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, requestBody any, responseBody any) error {
	return postJSONWithHeaders(ctx, client, endpoint, requestBody, responseBody, nil, "")
}

func postJSONWithHeaders(ctx context.Context, client *http.Client, endpoint string, requestBody any, responseBody any, headers map[string]string, redactedSecret string) error {
	bodyBytes, err := json.Marshal(requestBody)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		if strings.TrimSpace(name) != "" && value != "" {
			req.Header.Set(name, value)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s returned %d: %s", endpoint, resp.StatusCode, redactSecret(strings.TrimSpace(string(body)), redactedSecret))
	}

	if err := json.NewDecoder(resp.Body).Decode(responseBody); err != nil {
		return err
	}
	return nil
}

func redactSecret(text string, secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, "[REDACTED]")
}

func elapsedMillis(started time.Time) int {
	return durationMillis(time.Since(started))
}

func durationMillis(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	millis := int(duration / time.Millisecond)
	if millis == 0 {
		return 1
	}
	return millis
}

func metaLatencyMillis(meta map[string]any) int {
	if meta == nil {
		return 0
	}

	value, ok := meta["latency_ms"]
	if !ok {
		return 0
	}

	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case float32:
		return int(typed)
	case json.Number:
		asInt, err := typed.Int64()
		if err == nil {
			return int(asInt)
		}
		asFloat, err := typed.Float64()
		if err == nil {
			return int(asFloat)
		}
	case string:
		var number json.Number = json.Number(strings.TrimSpace(typed))
		asInt, err := number.Int64()
		if err == nil {
			return int(asInt)
		}
		asFloat, err := number.Float64()
		if err == nil {
			return int(asFloat)
		}
	}

	return 0
}

func autoCommitIdleMs(trigger string, idle time.Duration) int {
	if trigger != "auto_commit" && trigger != "auto_loopback" {
		return 0
	}
	return durationMillis(idle)
}

func cloneVoiceTurnTiming(timing *voiceTurnTiming) *voiceTurnTiming {
	if timing == nil {
		return nil
	}
	copy := *timing
	return &copy
}

func pcmuDuration(payload []byte) time.Duration {
	if len(payload) == 0 {
		return 0
	}
	return time.Duration(len(payload)) * time.Second / audioSampleRateHz
}

func autoCommitRMSGateEnabled(thresholdDB float64) bool {
	return thresholdDB > disableAutoCommitRMS
}

func pcmuRMSDBFS(payload []byte) float64 {
	if len(payload) == 0 {
		return math.Inf(-1)
	}

	var sumSquares float64
	for _, sample := range payload {
		pcm := float64(decodeULaw(sample))
		sumSquares += pcm * pcm
	}

	rms := math.Sqrt(sumSquares / float64(len(payload)))
	if rms <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(rms/32768)
}

func decodeULaw(sample byte) int16 {
	u := ^sample
	t := int((u&0x0F)<<3) + 0x84
	t <<= (u & 0x70) >> 4
	if u&0x80 != 0 {
		return int16(0x84 - t)
	}
	return int16(t - 0x84)
}

func synthesizeMockTone(text string) []byte {
	charCount := len(strings.TrimSpace(text))
	if charCount == 0 {
		charCount = 8
	}

	duration := 400*time.Millisecond + time.Duration(charCount)*25*time.Millisecond
	totalSamples := int(duration.Seconds() * audioSampleRateHz)
	if totalSamples < 1 {
		totalSamples = audioSampleRateHz / 2
	}

	audio := make([]byte, totalSamples)
	frequency := 440.0
	amplitude := 12000.0

	for i := 0; i < totalSamples; i++ {
		envelope := 1.0
		if i < audioSampleRateHz/20 {
			envelope = float64(i) / float64(audioSampleRateHz/20)
		}
		sample := int16(math.Sin(2*math.Pi*frequency*float64(i)/audioSampleRateHz) * amplitude * envelope)
		audio[i] = encodeULaw(sample)
	}

	return audio
}

func encodeULaw(sample int16) byte {
	const (
		ulawBias = 0x84
		ulawClip = 32635
	)

	pcm := int(sample)
	sign := 0
	if pcm < 0 {
		sign = 0x80
		pcm = -pcm
	}
	if pcm > ulawClip {
		pcm = ulawClip
	}

	pcm += ulawBias

	exponent := 7
	mask := 0x4000
	for exponent > 0 && (pcm&mask) == 0 {
		exponent--
		mask >>= 1
	}

	mantissa := (pcm >> (exponent + 3)) & 0x0F
	ulawByte := ^byte(sign | (exponent << 4) | mantissa)
	if ulawByte == 0 {
		ulawByte = 0x02
	}
	return ulawByte
}

func init() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}

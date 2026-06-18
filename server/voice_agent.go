package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
)

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

type voiceTurnResult struct {
	TurnID           int              `json:"turn_id"`
	CreatedAt        time.Time        `json:"created_at"`
	Source           string           `json:"source"`
	InputText        string           `json:"input_text"`
	ReplyText        string           `json:"reply_text"`
	InputAudioBytes  int              `json:"input_audio_bytes"`
	OutputAudioBytes int              `json:"output_audio_bytes"`
	Trace            []agentTraceStep `json:"trace,omitempty"`
}

type voiceStatus struct {
	SessionID          string            `json:"session_id"`
	BufferedAudioBytes int               `json:"buffered_audio_bytes"`
	Processing         bool              `json:"processing"`
	HistoryMessages    int               `json:"history_messages"`
	ESP32Endpoint      string            `json:"esp32_endpoint,omitempty"`
	Backends           map[string]string `json:"backends"`
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
	playAudio func([]byte) error

	mu       sync.Mutex
	buffer   []byte
	history  []chatMessage
	lastTurn *voiceTurnResult
	turnID   int
	busy     bool
}

func newVoiceAgentService(cfg config, asr ASRClient, agent AgentClient, tts TTSClient, playAudio func([]byte) error) *voiceAgentService {
	return &voiceAgentService{
		cfg:       cfg,
		asr:       asr,
		agent:     agent,
		tts:       tts,
		playAudio: playAudio,
	}
}

func (v *voiceAgentService) IngestAudio(payload []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.buffer = append(v.buffer, payload...)
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
}

func (v *voiceAgentService) CommitBufferedAudio(ctx context.Context, speak bool) (voiceTurnResult, error) {
	snapshot, err := v.beginBufferedTurn()
	if err != nil {
		return voiceTurnResult{}, err
	}

	req := asrRequest{
		SessionID: v.cfg.sessionID,
		AudioFormat: audioFormat{
			Encoding:     audioEncodingG711ULaw,
			SampleRateHz: audioSampleRateHz,
			Channels:     audioChannels,
		},
		AudioBase64: base64.StdEncoding.EncodeToString(snapshot),
	}

	asrResult, err := v.asr.Transcribe(ctx, req)
	if err != nil {
		v.restoreBufferedAudio(snapshot)
		return voiceTurnResult{}, err
	}

	text := strings.TrimSpace(asrResult.Text)
	if text == "" {
		v.restoreBufferedAudio(snapshot)
		return voiceTurnResult{}, fmt.Errorf("ASR returned empty text")
	}

	turn, err := v.executeTurn(ctx, "audio", text, len(snapshot), speak)
	if err != nil {
		v.restoreBufferedAudio(snapshot)
		return voiceTurnResult{}, err
	}

	v.finishTurn(&turn)
	return turn, nil
}

func (v *voiceAgentService) RunTextTurn(ctx context.Context, text string, speak bool) (voiceTurnResult, error) {
	if strings.TrimSpace(text) == "" {
		return voiceTurnResult{}, fmt.Errorf("text is required")
	}

	if err := v.beginTextTurn(); err != nil {
		return voiceTurnResult{}, err
	}

	turn, err := v.executeTurn(ctx, "text", text, 0, speak)
	if err != nil {
		v.failTurn()
		return voiceTurnResult{}, err
	}

	v.finishTurn(&turn)
	return turn, nil
}

func (v *voiceAgentService) beginBufferedTurn() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.busy {
		return nil, fmt.Errorf("voice agent already processing a turn")
	}
	if len(v.buffer) == 0 {
		return nil, fmt.Errorf("no buffered audio available")
	}

	snapshot := append([]byte(nil), v.buffer...)
	v.buffer = nil
	v.busy = true
	return snapshot, nil
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
	}
}

func (v *voiceAgentService) executeTurn(ctx context.Context, source string, text string, inputAudioBytes int, speak bool) (voiceTurnResult, error) {
	messages := v.buildMessages(text)
	agentReq := agentRequest{
		SessionID:       v.cfg.sessionID,
		SystemPrompt:    v.cfg.systemPrompt,
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

	agentResp, err := v.agent.Run(ctx, agentReq)
	if err != nil {
		return voiceTurnResult{}, err
	}

	replyText := strings.TrimSpace(agentResp.Text)
	if replyText == "" {
		return voiceTurnResult{}, fmt.Errorf("LLM returned empty reply")
	}

	synthResp, err := v.tts.Synthesize(ctx, ttsRequest{
		SessionID: v.cfg.sessionID,
		Text:      replyText,
		AudioFormat: audioFormat{
			Encoding:     audioEncodingG711ULaw,
			SampleRateHz: audioSampleRateHz,
			Channels:     audioChannels,
		},
	})
	if err != nil {
		return voiceTurnResult{}, err
	}

	if speak && len(synthResp.Audio) > 0 {
		if err := v.playAudio(synthResp.Audio); err != nil {
			return voiceTurnResult{}, err
		}
	}

	v.appendHistory(text, replyText)

	turn := voiceTurnResult{
		Source:           source,
		InputText:        text,
		ReplyText:        replyText,
		InputAudioBytes:  inputAudioBytes,
		OutputAudioBytes: len(synthResp.Audio),
		Trace:            agentResp.Trace,
	}
	return turn, nil
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
	bodyBytes, err := json.Marshal(requestBody)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s returned %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if err := json.NewDecoder(resp.Body).Decode(responseBody); err != nil {
		return err
	}
	return nil
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

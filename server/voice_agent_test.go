package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeASRClient struct {
	text         string
	err          error
	meta         map[string]any
	sleep        time.Duration
	lastAudioLen int
}

func (f *fakeASRClient) Transcribe(_ context.Context, req asrRequest) (asrResult, error) {
	if f.sleep > 0 {
		time.Sleep(f.sleep)
	}
	audio, err := base64.StdEncoding.DecodeString(req.AudioBase64)
	if err != nil {
		return asrResult{}, err
	}
	f.lastAudioLen = len(audio)
	if f.err != nil {
		return asrResult{}, f.err
	}
	return asrResult{Text: f.text, Final: true, Meta: f.meta}, nil
}

type fakeAgentClient struct {
	err   error
	sleep time.Duration
}

func (f fakeAgentClient) Run(_ context.Context, req agentRequest) (agentResult, error) {
	if f.sleep > 0 {
		time.Sleep(f.sleep)
	}
	if f.err != nil {
		return agentResult{}, f.err
	}

	userText := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			userText = req.Messages[i].Content
			break
		}
	}
	return agentResult{Text: "reply to " + userText, StopReason: "test_complete"}, nil
}

type fakeTTSClient struct {
	err   error
	meta  map[string]any
	sleep time.Duration
}

func (f fakeTTSClient) Synthesize(_ context.Context, req ttsRequest) (ttsResult, error) {
	if f.sleep > 0 {
		time.Sleep(f.sleep)
	}
	if f.err != nil {
		return ttsResult{}, f.err
	}
	return ttsResult{Audio: []byte("pcmu:" + req.Text), Meta: f.meta}, nil
}

type capturingAgentClient struct {
	lastRequest agentRequest
}

func (c *capturingAgentClient) Run(_ context.Context, req agentRequest) (agentResult, error) {
	c.lastRequest = req
	return agentResult{Text: "captured reply", StopReason: "test_complete"}, nil
}

func newTestVoiceAgent(asr ASRClient, agent AgentClient, tts TTSClient, playAudio func([]byte) error) *voiceAgentService {
	cfg := config{
		sessionID:          "test-session",
		systemPrompt:       "test prompt",
		asrBackend:         "test-asr",
		llmBackend:         "test-llm",
		ttsBackend:         "test-tts",
		ragTopK:            4,
		ragMaxContextChars: 5000,
		ragMinScore:        0.02,
		autoCommitIdle:     1500 * time.Millisecond,
		autoCommitMinBytes: 4,
		autoCommitMinRMSDB: -100,
	}
	return newVoiceAgentService(cfg, asr, agent, tts, nil, playAudio)
}

func TestRunAudioTurnExecutesASRLLMTTS(t *testing.T) {
	asr := &fakeASRClient{text: "hello from audio", meta: map[string]any{"latency_ms": 12}, sleep: time.Millisecond}
	voice := newTestVoiceAgent(
		asr,
		fakeAgentClient{sleep: time.Millisecond},
		fakeTTSClient{meta: map[string]any{"latency_ms": 34}, sleep: time.Millisecond},
		nil,
	)

	turn, err := voice.RunAudioTurn(context.Background(), []byte{1, 2, 3, 4}, false)
	if err != nil {
		t.Fatalf("RunAudioTurn returned error: %v", err)
	}

	if turn.Source != "audio" {
		t.Fatalf("Source = %q, want audio", turn.Source)
	}
	if turn.InputText != "hello from audio" {
		t.Fatalf("InputText = %q", turn.InputText)
	}
	if turn.ReplyText != "reply to hello from audio" {
		t.Fatalf("ReplyText = %q", turn.ReplyText)
	}
	if turn.InputAudioBytes != 4 {
		t.Fatalf("InputAudioBytes = %d, want 4", turn.InputAudioBytes)
	}
	if turn.OutputAudioBytes == 0 {
		t.Fatalf("OutputAudioBytes = 0, want non-zero")
	}
	if asr.lastAudioLen != 4 {
		t.Fatalf("ASR saw %d audio bytes, want 4", asr.lastAudioLen)
	}
	if turn.Timing == nil {
		t.Fatalf("Timing is nil")
	}
	if turn.Timing.Trigger != "audio_turn" {
		t.Fatalf("Timing.Trigger = %q, want audio_turn", turn.Timing.Trigger)
	}
	if turn.Timing.TotalMs == 0 || turn.Timing.ASRTotalMs == 0 || turn.Timing.LLMTotalMs == 0 || turn.Timing.TTSTotalMs == 0 {
		t.Fatalf("Timing missing total stage durations: %#v", turn.Timing)
	}
	if turn.Timing.ASRBackendMs != 12 {
		t.Fatalf("ASRBackendMs = %d, want 12", turn.Timing.ASRBackendMs)
	}
	if turn.Timing.TTSBackendMs != 34 {
		t.Fatalf("TTSBackendMs = %d, want 34", turn.Timing.TTSBackendMs)
	}

	status := voice.Status("")
	if status.Processing {
		t.Fatalf("Processing = true after completed turn")
	}
	if status.HistoryMessages != 2 {
		t.Fatalf("HistoryMessages = %d, want 2", status.HistoryMessages)
	}
	if status.LastTurn == nil || status.LastTurn.TurnID != 1 {
		t.Fatalf("LastTurn not populated correctly: %#v", status.LastTurn)
	}
	if status.LastTurn.Timing == nil || status.LastTurn.Timing.Trigger != "audio_turn" {
		t.Fatalf("LastTurn timing not populated correctly: %#v", status.LastTurn.Timing)
	}
}

func TestRunTextTurnBuildsBlueSeeSystemPromptWithoutRAG(t *testing.T) {
	agent := &capturingAgentClient{}
	cfg := config{
		sessionID:          "test-session",
		systemPrompt:       "Keep answers under two sentences.",
		asrBackend:         "test-asr",
		llmBackend:         "test-llm",
		ttsBackend:         "test-tts",
		ragEnable:          false,
		ragTopK:            4,
		ragMaxContextChars: 5000,
		ragMinScore:        0.02,
	}
	voice := newVoiceAgentService(cfg, &fakeASRClient{text: "unused"}, agent, fakeTTSClient{}, nil, nil)

	turn, err := voice.RunTextTurn(context.Background(), "What can you do?", false)
	if err != nil {
		t.Fatalf("RunTextTurn returned error: %v", err)
	}
	if turn.ReplyText != "captured reply" {
		t.Fatalf("ReplyText = %q, want captured reply", turn.ReplyText)
	}
	if !strings.Contains(agent.lastRequest.SystemPrompt, "You are BlueSee") {
		t.Fatalf("system prompt missing BlueSee identity: %q", agent.lastRequest.SystemPrompt)
	}
	if !strings.Contains(agent.lastRequest.SystemPrompt, "Keep answers under two sentences.") {
		t.Fatalf("system prompt missing operator instruction: %q", agent.lastRequest.SystemPrompt)
	}
	if strings.Contains(agent.lastRequest.SystemPrompt, "Retrieved campus context") {
		t.Fatalf("system prompt included RAG context while disabled: %q", agent.lastRequest.SystemPrompt)
	}
	if len(agent.lastRequest.Messages) != 1 || agent.lastRequest.Messages[0].Content != "What can you do?" {
		t.Fatalf("messages not forwarded correctly: %#v", agent.lastRequest.Messages)
	}
}

func TestRunTextTurnInjectsRAGContextAndStatus(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "blueSee.md"), []byte(`# The Hub
The Hub is a general first contact point for students who are unsure which university service can help.

## FoSE Support
FoSE students and staff can ask BlueSee for Faculty of Science and Engineering orientation, but programme rules should be confirmed with official FoSE or administrative channels.
`), 0644); err != nil {
		t.Fatalf("write knowledge file: %v", err)
	}
	rag, err := loadRAGStore(dir)
	if err != nil {
		t.Fatalf("loadRAGStore returned error: %v", err)
	}

	agent := &capturingAgentClient{}
	cfg := config{
		sessionID:          "test-session",
		systemPrompt:       "test prompt",
		asrBackend:         "test-asr",
		llmBackend:         "test-llm",
		ttsBackend:         "test-tts",
		ragEnable:          true,
		ragTopK:            2,
		ragMaxContextChars: 1000,
		ragMinScore:        0.01,
	}
	voice := newVoiceAgentService(cfg, &fakeASRClient{text: "unused"}, agent, fakeTTSClient{}, rag, nil)

	turn, err := voice.RunTextTurn(context.Background(), "Where should a FoSE student ask for help?", false)
	if err != nil {
		t.Fatalf("RunTextTurn returned error: %v", err)
	}
	if !strings.Contains(agent.lastRequest.SystemPrompt, "Retrieved campus context") {
		t.Fatalf("system prompt missing RAG context: %q", agent.lastRequest.SystemPrompt)
	}
	if !strings.Contains(agent.lastRequest.SystemPrompt, "The Hub is a general first contact point") {
		t.Fatalf("system prompt missing retrieved content: %q", agent.lastRequest.SystemPrompt)
	}
	if len(turn.Trace) == 0 || turn.Trace[0].Type != "rag" || !strings.Contains(turn.Trace[0].Summary, ragRouteContext) {
		t.Fatalf("turn trace missing RAG step: %#v", turn.Trace)
	}

	status := voice.Status("")
	if !status.RAGEnabled {
		t.Fatalf("RAGEnabled = false, want true")
	}
	if status.RAGFiles != 1 || status.RAGSections != 2 {
		t.Fatalf("RAG stats = files %d sections %d, want 1/2", status.RAGFiles, status.RAGSections)
	}
}

func TestRunAudioTurnRejectsEmptyAudio(t *testing.T) {
	voice := newTestVoiceAgent(&fakeASRClient{text: "unused"}, fakeAgentClient{}, fakeTTSClient{}, nil)

	_, err := voice.RunAudioTurn(context.Background(), nil, false)
	if err == nil || !strings.Contains(err.Error(), "audio is required") {
		t.Fatalf("RunAudioTurn error = %v, want audio is required", err)
	}
}

func TestCommitBufferedAudioPreservesBufferOnFailure(t *testing.T) {
	voice := newTestVoiceAgent(
		&fakeASRClient{err: errors.New("asr unavailable")},
		fakeAgentClient{},
		fakeTTSClient{},
		nil,
	)
	voice.IngestAudio([]byte{9, 8, 7})

	_, err := voice.CommitBufferedAudio(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "asr unavailable") {
		t.Fatalf("CommitBufferedAudio error = %v, want asr unavailable", err)
	}

	status := voice.Status("")
	if status.Processing {
		t.Fatalf("Processing = true after failed turn")
	}
	if status.BufferedAudioBytes != 3 {
		t.Fatalf("BufferedAudioBytes = %d, want 3", status.BufferedAudioBytes)
	}
}

func TestCommitBufferedAudioRequiresBuffer(t *testing.T) {
	voice := newTestVoiceAgent(&fakeASRClient{text: "unused"}, fakeAgentClient{}, fakeTTSClient{}, nil)

	_, err := voice.CommitBufferedAudio(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "no buffered audio available") {
		t.Fatalf("CommitBufferedAudio error = %v, want no buffered audio available", err)
	}
}

func TestCommitBufferedAudioDropsBufferOnEmptyASR(t *testing.T) {
	voice := newTestVoiceAgent(
		&fakeASRClient{text: ""},
		fakeAgentClient{},
		fakeTTSClient{},
		nil,
	)
	voice.IngestAudio([]byte{9, 8, 7, 6})

	_, err := voice.CommitBufferedAudioAuto(context.Background(), false)
	if !errors.Is(err, errASREmptyText) {
		t.Fatalf("CommitBufferedAudioAuto error = %v, want errASREmptyText", err)
	}

	status := voice.Status("")
	if status.Processing {
		t.Fatalf("Processing = true after empty ASR")
	}
	if status.BufferedAudioBytes != 0 {
		t.Fatalf("BufferedAudioBytes = %d, want 0", status.BufferedAudioBytes)
	}
}

func TestCommitBufferedAudioAutoReportsTriggerAndPlaybackTiming(t *testing.T) {
	voice := newTestVoiceAgent(
		&fakeASRClient{text: "auto audio", meta: map[string]any{"latency_ms": json.Number("23")}},
		fakeAgentClient{sleep: time.Millisecond},
		fakeTTSClient{meta: map[string]any{"latency_ms": float64(45)}, sleep: time.Millisecond},
		func(_ []byte) error {
			time.Sleep(time.Millisecond)
			return nil
		},
	)
	voice.IngestAudio([]byte{1, 2, 3, 4})

	turn, err := voice.CommitBufferedAudioAuto(context.Background(), true)
	if err != nil {
		t.Fatalf("CommitBufferedAudioAuto returned error: %v", err)
	}
	if turn.Timing == nil {
		t.Fatalf("Timing is nil")
	}
	if turn.Timing.Trigger != "auto_commit" {
		t.Fatalf("Trigger = %q, want auto_commit", turn.Timing.Trigger)
	}
	if turn.Timing.AutoCommitIdleMs != 1500 {
		t.Fatalf("AutoCommitIdleMs = %d, want 1500", turn.Timing.AutoCommitIdleMs)
	}
	if turn.Timing.PlaybackSendMs == 0 {
		t.Fatalf("PlaybackSendMs = 0, want non-zero")
	}
	if turn.Timing.ASRBackendMs != 23 {
		t.Fatalf("ASRBackendMs = %d, want 23", turn.Timing.ASRBackendMs)
	}
	if turn.Timing.TTSBackendMs != 45 {
		t.Fatalf("TTSBackendMs = %d, want 45", turn.Timing.TTSBackendMs)
	}
}

func TestLoopbackBufferedAudioSendsRawBuffer(t *testing.T) {
	var played []byte
	voice := newTestVoiceAgent(
		&fakeASRClient{text: "should not be called"},
		fakeAgentClient{},
		fakeTTSClient{},
		func(audio []byte) error {
			time.Sleep(time.Millisecond)
			played = append([]byte(nil), audio...)
			return nil
		},
	)
	voice.IngestAudio([]byte{1, 2, 3, 4, 5, 6})

	turn, err := voice.LoopbackBufferedAudio(context.Background(), "loopback")
	if err != nil {
		t.Fatalf("LoopbackBufferedAudio returned error: %v", err)
	}

	if turn.Source != "loopback" {
		t.Fatalf("Source = %q, want loopback", turn.Source)
	}
	if turn.InputAudioBytes != 6 || turn.OutputAudioBytes != 6 {
		t.Fatalf("loopback byte counts = input %d output %d, want 6/6", turn.InputAudioBytes, turn.OutputAudioBytes)
	}
	if string(played) != string([]byte{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("played bytes = %v", played)
	}
	if turn.Timing == nil || turn.Timing.Trigger != "loopback" || turn.Timing.PlaybackSendMs == 0 {
		t.Fatalf("loopback timing not populated correctly: %#v", turn.Timing)
	}

	status := voice.Status("")
	if status.BufferedAudioBytes != 0 {
		t.Fatalf("BufferedAudioBytes = %d, want 0", status.BufferedAudioBytes)
	}
}

func TestLoopbackBufferedAudioRestoresBufferOnPlaybackFailure(t *testing.T) {
	voice := newTestVoiceAgent(
		&fakeASRClient{text: "should not be called"},
		fakeAgentClient{},
		fakeTTSClient{},
		func(_ []byte) error {
			return errors.New("speaker unavailable")
		},
	)
	voice.IngestAudio([]byte{1, 2, 3, 4})

	_, err := voice.LoopbackBufferedAudio(context.Background(), "loopback")
	if err == nil || !strings.Contains(err.Error(), "speaker unavailable") {
		t.Fatalf("LoopbackBufferedAudio error = %v, want speaker unavailable", err)
	}

	status := voice.Status("")
	if status.BufferedAudioBytes != 4 {
		t.Fatalf("BufferedAudioBytes = %d, want 4", status.BufferedAudioBytes)
	}
}

func TestShouldAutoCommitDropsLowEnergyBufferedAudio(t *testing.T) {
	cfg := config{
		sessionID:          "test-session",
		systemPrompt:       "test prompt",
		asrBackend:         "test-asr",
		llmBackend:         "test-llm",
		ttsBackend:         "test-tts",
		autoCommitIdle:     1500 * time.Millisecond,
		autoCommitMinBytes: 4000,
		autoCommitMinAudio: 800 * time.Millisecond,
		autoCommitMinRMSDB: -45,
	}
	voice := newVoiceAgentService(cfg, &fakeASRClient{text: "unused"}, fakeAgentClient{}, fakeTTSClient{}, nil, nil)
	voice.IngestAudio(bytesOf(0xFF, 8000))

	if voice.ShouldAutoCommit(time.Now().Add(2 * time.Second)) {
		t.Fatalf("ShouldAutoCommit = true for low-energy silence")
	}

	status := voice.Status("")
	if status.BufferedAudioBytes != 0 {
		t.Fatalf("BufferedAudioBytes = %d, want low-energy buffer discarded", status.BufferedAudioBytes)
	}
}

func TestShouldAutoCommitHonorsIdleAndMinBytes(t *testing.T) {
	voice := newTestVoiceAgent(&fakeASRClient{text: "unused"}, fakeAgentClient{}, fakeTTSClient{}, nil)
	now := time.Now()

	if voice.ShouldAutoCommit(now) {
		t.Fatalf("ShouldAutoCommit = true without audio")
	}

	voice.IngestAudio([]byte{1, 2, 3})
	if voice.ShouldAutoCommit(now.Add(2 * time.Second)) {
		t.Fatalf("ShouldAutoCommit = true below min bytes")
	}

	voice.IngestAudio([]byte{4})
	if voice.ShouldAutoCommit(time.Now().Add(500 * time.Millisecond)) {
		t.Fatalf("ShouldAutoCommit = true before idle duration")
	}
	if !voice.ShouldAutoCommit(time.Now().Add(2 * time.Second)) {
		t.Fatalf("ShouldAutoCommit = false after idle duration and min bytes")
	}
}

func TestShouldAutoCommitSkipsBusyTurn(t *testing.T) {
	voice := newTestVoiceAgent(&fakeASRClient{text: "unused"}, fakeAgentClient{}, fakeTTSClient{}, nil)
	voice.IngestAudio([]byte{1, 2, 3, 4})

	if err := voice.beginTextTurn(); err != nil {
		t.Fatalf("beginTextTurn returned error: %v", err)
	}

	if voice.ShouldAutoCommit(time.Now().Add(2 * time.Second)) {
		t.Fatalf("ShouldAutoCommit = true while busy")
	}
}

func TestOpenAICompatibleAgentClientSendsBearerAuth(t *testing.T) {
	var gotAuth string
	var gotModel string
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", req.Method)
		}
		gotAuth = req.Header.Get("Authorization")

		var payload struct {
			Model    string        `json:"model"`
			Messages []chatMessage `json:"messages"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotModel = payload.Model
		if len(payload.Messages) != 2 {
			t.Fatalf("messages len = %d, want 2", len(payload.Messages))
		}
		if payload.Messages[0].Role != "system" || payload.Messages[1].Role != "user" {
			t.Fatalf("unexpected messages: %#v", payload.Messages)
		}

		writeJSON(rw, http.StatusOK, map[string]any{
			"choices": []map[string]any{
				{
					"message":       map[string]string{"content": "cloud reply"},
					"finish_reason": "stop",
				},
			},
		})
	}))
	defer server.Close()

	client := openAICompatibleAgentClient{
		endpoint:  server.URL,
		model:     "deepseek-v4-flash",
		maxTokens: 128,
		apiKey:    "test-secret",
		client:    server.Client(),
	}

	result, err := client.Run(context.Background(), agentRequest{
		SystemPrompt: "system prompt",
		Messages:     []chatMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if gotAuth != "Bearer test-secret" {
		t.Fatalf("Authorization = %q, want bearer token", gotAuth)
	}
	if gotModel != "deepseek-v4-flash" {
		t.Fatalf("model = %q", gotModel)
	}
	if result.Text != "cloud reply" || result.StopReason != "stop" {
		t.Fatalf("result = %#v", result)
	}
}

func TestOpenAICompatibleAgentClientOmitsEmptyBearerAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if gotAuth := req.Header.Get("Authorization"); gotAuth != "" {
			t.Fatalf("Authorization = %q, want empty", gotAuth)
		}
		writeJSON(rw, http.StatusOK, map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "reply"}},
			},
		})
	}))
	defer server.Close()

	client := openAICompatibleAgentClient{
		endpoint: server.URL,
		model:    "local-model",
		client:   server.Client(),
	}

	if _, err := client.Run(context.Background(), agentRequest{Messages: []chatMessage{{Role: "user", Content: "hello"}}}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestOpenAICompatibleAgentClientRedactsAPIKeyFromHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		writeJSON(rw, http.StatusUnauthorized, map[string]any{"error": "bad key test-secret"})
	}))
	defer server.Close()

	client := openAICompatibleAgentClient{
		endpoint: server.URL,
		model:    "deepseek-v4-flash",
		apiKey:   "test-secret",
		client:   server.Client(),
	}

	_, err := client.Run(context.Background(), agentRequest{Messages: []chatMessage{{Role: "user", Content: "hello"}}})
	if err == nil {
		t.Fatalf("Run returned nil error")
	}
	if strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("error leaked api key: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error = %v, want redaction marker", err)
	}
}

func TestOpenAICompatibleAgentClientRejectsReasoningOnlyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		writeJSON(rw, http.StatusOK, map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]string{
						"content":           "",
						"reasoning_content": "thinking only",
					},
				},
			},
		})
	}))
	defer server.Close()

	client := openAICompatibleAgentClient{
		endpoint: server.URL,
		model:    "cloud-model",
		client:   server.Client(),
	}

	_, err := client.Run(context.Background(), agentRequest{Messages: []chatMessage{{Role: "user", Content: "hello"}}})
	if err == nil || !strings.Contains(err.Error(), "reasoning_content") {
		t.Fatalf("Run error = %v, want reasoning_content error", err)
	}
}

func bytesOf(value byte, count int) []byte {
	payload := make([]byte, count)
	for i := range payload {
		payload[i] = value
	}
	return payload
}

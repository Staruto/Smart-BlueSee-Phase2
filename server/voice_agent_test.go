package main

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

type fakeASRClient struct {
	text         string
	err          error
	lastAudioLen int
}

func (f *fakeASRClient) Transcribe(_ context.Context, req asrRequest) (asrResult, error) {
	audio, err := base64.StdEncoding.DecodeString(req.AudioBase64)
	if err != nil {
		return asrResult{}, err
	}
	f.lastAudioLen = len(audio)
	if f.err != nil {
		return asrResult{}, f.err
	}
	return asrResult{Text: f.text, Final: true}, nil
}

type fakeAgentClient struct {
	err error
}

func (f fakeAgentClient) Run(_ context.Context, req agentRequest) (agentResult, error) {
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
	err error
}

func (f fakeTTSClient) Synthesize(_ context.Context, req ttsRequest) (ttsResult, error) {
	if f.err != nil {
		return ttsResult{}, f.err
	}
	return ttsResult{Audio: []byte("pcmu:" + req.Text)}, nil
}

func newTestVoiceAgent(asr ASRClient, agent AgentClient, tts TTSClient, playAudio func([]byte) error) *voiceAgentService {
	cfg := config{
		sessionID:    "test-session",
		systemPrompt: "test prompt",
		asrBackend:   "test-asr",
		llmBackend:   "test-llm",
		ttsBackend:   "test-tts",
	}
	return newVoiceAgentService(cfg, asr, agent, tts, playAudio)
}

func TestRunAudioTurnExecutesASRLLMTTS(t *testing.T) {
	asr := &fakeASRClient{text: "hello from audio"}
	voice := newTestVoiceAgent(asr, fakeAgentClient{}, fakeTTSClient{}, nil)

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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

type voiceTurnRequest struct {
	Text  string `json:"text"`
	Speak *bool  `json:"speak,omitempty"`
}

func (a *serverApp) handleHealthz(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeMethodNotAllowed(rw, http.MethodGet)
		return
	}

	writeJSON(rw, http.StatusOK, map[string]any{
		"ok":               true,
		"webrtc_enabled":   a.webrtc != nil,
		"voice_enabled":    a.voice != nil,
		"esp32_endpoint":   a.udp.ESP32Endpoint(),
		"voice_session_id": a.cfg.sessionID,
	})
}

func (a *serverApp) handleVoiceStatus(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeMethodNotAllowed(rw, http.MethodGet)
		return
	}

	writeJSON(rw, http.StatusOK, a.voice.Status(a.udp.ESP32Endpoint()))
}

func (a *serverApp) handleVoiceCommit(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeMethodNotAllowed(rw, http.MethodPost)
		return
	}

	var payload voiceTurnRequest
	if err := decodeOptionalJSON(req, &payload); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), 90*time.Second)
	defer cancel()

	turn, err := a.voice.CommitBufferedAudio(ctx, boolOrDefault(payload.Speak, true))
	if err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(rw, http.StatusOK, turn)
}

func (a *serverApp) handleVoiceTextTurn(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeMethodNotAllowed(rw, http.MethodPost)
		return
	}

	var payload voiceTurnRequest
	if err := decodeOptionalJSON(req, &payload); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), 90*time.Second)
	defer cancel()

	turn, err := a.voice.RunTextTurn(ctx, payload.Text, boolOrDefault(payload.Speak, true))
	if err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(rw, http.StatusOK, turn)
}

func (a *serverApp) handleVoiceReset(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeMethodNotAllowed(rw, http.MethodPost)
		return
	}

	a.voice.Reset()
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

func decodeOptionalJSON(req *http.Request, target any) error {
	if req.Body == nil {
		return nil
	}
	defer req.Body.Close()

	decoder := json.NewDecoder(req.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(target)
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func writeJSON(rw http.ResponseWriter, status int, payload any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(payload)
}

func writeMethodNotAllowed(rw http.ResponseWriter, allowed string) {
	rw.Header().Set("Allow", allowed)
	writeJSON(rw, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
}

func boolOrDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

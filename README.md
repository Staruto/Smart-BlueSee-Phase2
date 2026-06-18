
# WebRTC Voice Agent Prototype

This repo now contains two server-side paths:

- `ESP32 <-> Cloud <-> Browser` via the existing WebRTC bridge
- `ESP32 mic -> Cloud -> ASR -> LLM -> TTS -> Cloud -> ESP32 speaker` via the new voice-agent pipeline

The voice-agent pipeline is intentionally modular:

- ASR, LLM, and TTS are separate backends
- each backend can run in `mock` mode for transport validation or `http` mode for real local modules
- the LLM request format already includes room for future MCP/tool-loop execution

## Server quick start

```powershell
cd server
$env:GOCACHE="$PWD\\..\\.gocache"
go run .
```

Default behavior:

- UDP listener on `:5000`
- HTTP server on `:8080`
- WebRTC bridge enabled
- Voice-agent APIs enabled
- `mock` ASR / LLM / TTS enabled

## Voice-agent API

- `GET /healthz`
- `GET /api/voice/status`
- `POST /api/voice/commit`
- `POST /api/voice/text-turn`
- `POST /api/voice/reset`

Example direct text turn:

```powershell
Invoke-RestMethod `
  -Method Post `
  -Uri http://127.0.0.1:8080/api/voice/text-turn `
  -ContentType 'application/json' `
  -Body '{"text":"hello from local test","speak":false}'
```

Example buffered audio commit:

```powershell
Invoke-RestMethod `
  -Method Post `
  -Uri http://127.0.0.1:8080/api/voice/commit `
  -ContentType 'application/json' `
  -Body '{"speak":true}'
```

The buffered commit endpoint uses whatever PCMU audio the ESP32 has already sent to the server over UDP.

## Backend modes

Environment variables:

- `VOICE_AGENT_ASR_BACKEND=mock|http`
- `VOICE_AGENT_LLM_BACKEND=mock|http|openai`
- `VOICE_AGENT_TTS_BACKEND=mock|http`
- `VOICE_AGENT_ASR_ENDPOINT=http://127.0.0.1:8091/transcribe`
- `VOICE_AGENT_LLM_ENDPOINT=http://127.0.0.1:8092/respond`
- `VOICE_AGENT_TTS_ENDPOINT=http://127.0.0.1:8093/synthesize`
- `VOICE_AGENT_LLM_MODEL=local-model`

`mock` mode validates server orchestration without requiring external modules. The mock TTS sends a generated PCMU tone, not spoken speech.

## Module contracts

ASR request:

```json
{
  "session_id": "esp32-default",
  "audio_format": {
    "encoding": "g711_ulaw",
    "sample_rate_hz": 8000,
    "channels": 1
  },
  "audio_base64": "..."
}
```

ASR response:

```json
{
  "text": "transcribed text",
  "final": true
}
```

LLM request:

```json
{
  "session_id": "esp32-default",
  "system_prompt": "You are a concise voice assistant.",
  "messages": [
    {"role": "user", "content": "hello"}
  ],
  "tools": [
    {"name": "future_web_search", "description": "Reserved placeholder for a future MCP-backed web search tool"}
  ],
  "enable_tool_calls": false,
  "max_steps": 1
}
```

LLM response:

```json
{
  "text": "short spoken answer",
  "stop_reason": "complete",
  "trace": [
    {"step": 1, "type": "reasoning", "summary": "Answered directly"}
  ]
}
```

TTS request:

```json
{
  "session_id": "esp32-default",
  "text": "short spoken answer",
  "audio_format": {
    "encoding": "g711_ulaw",
    "sample_rate_hz": 8000,
    "channels": 1
  }
}
```

TTS response:

```json
{
  "audio_base64": "..."
}
```

## Existing service commands

```bash
sudo systemctl status webrtc-client
sudo systemctl start webrtc-client

sudo systemctl status nginx
sudo systemctl start nginx

screen -ls
screen -S ngrok
ngrok http http://127.0.0.1:80

sudo journalctl -u webrtc-client -n 20 --no-pager
```

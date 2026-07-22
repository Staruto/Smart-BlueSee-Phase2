
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
- `POST /api/voice/loopback`
- `POST /api/voice/audio-turn`
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

Example buffered raw loopback:

```powershell
Invoke-RestMethod `
  -Method Post `
  -Uri http://127.0.0.1:8080/api/voice/loopback
```

The loopback endpoint skips ASR, LLM, and TTS. It sends the exact buffered ESP32 PCMU audio back to the current ESP32 UDP endpoint so you can check whether capture and transport contain the full utterance before ASR runs.

Example software-only audio turn:

```powershell
Invoke-RestMethod `
  -Method Post `
  -Uri http://127.0.0.1:8080/api/voice/audio-turn `
  -ContentType 'application/json' `
  -Body '{"audio_base64":"...","speak":false}'
```

The audio turn endpoint accepts base64 PCMU (`g711_ulaw`, 8 kHz, mono) and runs `ASR -> LLM -> TTS` without requiring ESP32 UDP audio. Use `speak:false` while ESP32 is offline.

Turn responses include a `timing` object for latency debugging. Key fields are:

- `total_ms`
- `asr_total_ms`, `asr_backend_ms`
- `llm_total_ms`
- `tts_total_ms`, `tts_backend_ms`
- `playback_send_ms`
- `buffer_age_ms`
- `trigger`

## Backend modes

Environment variables:

- `VOICE_AGENT_ASR_BACKEND=mock|http`
- `VOICE_AGENT_LLM_BACKEND=mock|http|openai`
- `VOICE_AGENT_TTS_BACKEND=mock|http`
- `VOICE_AGENT_ASR_ENDPOINT=http://127.0.0.1:8091/transcribe`
- `VOICE_AGENT_LLM_ENDPOINT=http://127.0.0.1:8092/respond`
- `VOICE_AGENT_TTS_ENDPOINT=http://127.0.0.1:8093/synthesize`
- `VOICE_AGENT_LLM_MODEL=local-model`
- `VOICE_AGENT_AUTO_COMMIT=false`
- `VOICE_AGENT_AUTO_COMMIT_MODE=agent`
- `VOICE_AGENT_AUTO_COMMIT_IDLE=1500ms`
- `VOICE_AGENT_AUTO_COMMIT_MIN_BYTES=4000`
- `VOICE_AGENT_AUTO_COMMIT_MIN_AUDIO=800ms`
- `VOICE_AGENT_AUTO_COMMIT_MIN_RMS_DB=-45`
- `VOICE_AGENT_AUTO_COMMIT_POLL=200ms`

`mock` mode validates server orchestration without requiring external modules. The mock TTS sends a generated PCMU tone, not spoken speech.

Auto-commit can be enabled after ESP32 VAD-gated audio upload is stable. When enabled in `agent` mode, the server commits buffered ESP32 audio after no new audio arrives for the configured idle duration, then sends TTS playback to the ESP32. In `loopback` mode, the server sends the raw buffered audio back to ESP32 instead of calling ASR, LLM, or TTS. The server also applies a secondary guard before committing: the buffer must satisfy the minimum byte count, minimum audio duration, and minimum RMS level. Set `VOICE_AGENT_AUTO_COMMIT_MIN_RMS_DB=-100` or lower to disable the RMS guard during debugging.

## Local ASR/TTS adapter

Use `tools/local_voice_adapter.py` when ASR and TTS run on the PC in the sibling `client` repo. The adapter exposes the HTTP contracts expected by the Go server and converts between PCMU 8 kHz and the local Python modules' PCM16 APIs.

Start the adapter on the PC:

```powershell
python tools\local_voice_adapter.py `
  --host 127.0.0.1 `
  --port 8094 `
  --client-dir C:\x\void\llm\client `
  --preload
```

Validate it directly:

```powershell
Invoke-RestMethod http://127.0.0.1:8094/healthz

Invoke-RestMethod `
  -Method Post `
  -Uri http://127.0.0.1:8094/synthesize `
  -ContentType 'application/json' `
  -Body '{"session_id":"local-test","text":"hello from local t t s","audio_format":{"encoding":"g711_ulaw","sample_rate_hz":8000,"channels":1}}'
```

Point the Go server at the local adapter:

```powershell
$env:VOICE_AGENT_ASR_BACKEND="http"
$env:VOICE_AGENT_ASR_ENDPOINT="http://127.0.0.1:8094/transcribe"
$env:VOICE_AGENT_TTS_BACKEND="http"
$env:VOICE_AGENT_TTS_ENDPOINT="http://127.0.0.1:8094/synthesize"
```

For cloud-to-PC validation, replace `127.0.0.1:8094` with the same server-reachable tunnel or VPN address pattern used for the real LLM.

Software-only validation order:

1. Start the real LLM, local ASR/TTS adapter, and Go server on the PC.
2. Call `/api/voice/text-turn` with `speak:false` to validate `LLM -> TTS`.
3. Call adapter `/synthesize` to create base64 PCMU for a known phrase.
4. Submit that base64 PCMU to `/api/voice/audio-turn` with `speak:false` to validate `ASR -> LLM -> TTS`.
5. Move the Go server to cloud and repeat `/api/voice/text-turn` and `/api/voice/audio-turn` using server-reachable PC-local module URLs.

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

# Deployment and Validation

This document describes how to deploy and validate the current project.

Current validated capabilities:

- `ESP32 <-> Cloud <-> Browser` audio transport over UDP + WebRTC
- `ESP32 mic -> Cloud -> ASR -> LLM -> TTS -> Cloud -> ESP32 speaker` over the server-side manual-turn voice pipeline

Current constraints:

- single public Ubuntu host
- single-session prototype behavior
- manual turn commit for the voice path
- no VAD, TURN, authentication, or multi-device routing

## Topology

The current runtime is split between a local workstation and the cloud server.

Local workstation responsibilities:

- build the Go server binary
- flash the ESP32 firmware
- optionally run a browser for WebRTC transport checks
- optionally run ASR / LLM / TTS modules if the cloud server can reach them over HTTP

Cloud server responsibilities:

- run the Go server binary
- receive ESP32 UDP audio on `5000/udp`
- expose browser assets through Nginx on `80/tcp`
- proxy `/ws` to the Go server for WebRTC signaling
- call ASR / LLM / TTS HTTP endpoints from the Go server when the voice pipeline is enabled

Important network rule:

- ASR / LLM / TTS are not embedded in the Go server. The Go server makes outbound HTTP calls to those modules.
- If those modules run on the same Ubuntu host, use `127.0.0.1`.
- If they run on your local PC, the Ubuntu host must be able to reach them through a routable IP or tunnel. Plain `127.0.0.1` on your PC will not work from the server.

## 1. Local Preparation

### 1.1 Check the repo and build the server

From the repo root on your local machine:

```powershell
cd server
$env:GOCACHE="$PWD\\..\\.gocache"
go test ./...
go build -o webrtc_server .
```

This produces the server binary at `server/webrtc_server` on Linux targets or `server/webrtc_server.exe` on Windows.

If you are building on Windows for an Ubuntu server, build on Linux or cross-compile separately before copying the binary.

### 1.2 Prepare the web assets

The `web/` directory is served by Nginx, not by the systemd service.

Files to deploy:

- Go binary from `server/`
- static browser assets from `web/`
- service file from `deploy/systemd/webrtc-client.service`
- Nginx config from `deploy/nginx/webrtc_client.conf`

### 1.3 Configure the ESP32 firmware

Before flashing, update the hardcoded network values in `mcu/src/main.c`:

- `WIFI_SSID`
- `WIFI_PASS`
- `UDP_SERVER_IP`
- `UDP_SERVER_PORT`

Right now the firmware still contains:

- Wi-Fi SSID: `yuhao`
- Wi-Fi password: `12345678`
- server IP: `117.72.24.77`
- UDP port: `5000`

Those values must match your real Wi-Fi network and the public IP of your Ubuntu server.

### 1.4 Flash the ESP32

From `mcu/` on your local machine:

```powershell
platformio run
platformio run --target upload
```

Use a serial monitor if needed to confirm the board connects to Wi-Fi and starts its UDP task.

## 2. Server Deployment

These steps assume an Ubuntu host reachable by the ESP32 over the public internet.

### 2.1 Create directories on the server

Example layout:

- `/opt/webrtc-client/bin/webrtc_server`
- `/opt/webrtc-client/current/`
- `/var/www/webrtc_client/`

Example commands:

```bash
sudo mkdir -p /opt/webrtc-client/bin
sudo mkdir -p /opt/webrtc-client/current
sudo mkdir -p /var/www/webrtc_client
```

### 2.2 Copy artifacts to the server

Copy these from your local machine to the Ubuntu host:

- `server/webrtc_server` -> `/opt/webrtc-client/bin/webrtc_server`
- `web/*` -> `/var/www/webrtc_client/`
- `deploy/systemd/webrtc-client.service` -> `/etc/systemd/system/webrtc-client.service`
- `deploy/nginx/webrtc_client.conf` -> `/etc/nginx/sites-available/webrtc_client.conf`

If you use `scp`, the commands will look like:

```bash
scp server/webrtc_server user@your-server:/tmp/webrtc_server
scp -r web/* user@your-server:/tmp/webrtc_web/
scp deploy/systemd/webrtc-client.service user@your-server:/tmp/webrtc-client.service
scp deploy/nginx/webrtc_client.conf user@your-server:/tmp/webrtc_client.conf
```

Then move them into place on the server with `sudo`.

### 2.3 Install the Go server binary

On the Ubuntu host:

```bash
sudo install -m 0755 /tmp/webrtc_server /opt/webrtc-client/bin/webrtc_server
sudo chown -R root:root /opt/webrtc-client/bin
```

### 2.4 Install the web assets

On the Ubuntu host:

```bash
sudo rsync -av --delete /tmp/webrtc_web/ /var/www/webrtc_client/
```

### 2.5 Install and enable systemd

The provided service runs:

- Go server on `127.0.0.1:8080`
- UDP listener on `:5000`
- no static serving from the Go process because `-web-dir=` is passed

Create the service account and enable the service:

```bash
sudo useradd --system --home /opt/webrtc-client --shell /usr/sbin/nologin webrtc
sudo install -m 0644 /tmp/webrtc-client.service /etc/systemd/system/webrtc-client.service
sudo systemctl daemon-reload
sudo systemctl enable --now webrtc-client
sudo systemctl status webrtc-client
```

### 2.6 Configure the voice pipeline environment

By default the server uses mock backends:

- `VOICE_AGENT_ASR_BACKEND=mock`
- `VOICE_AGENT_LLM_BACKEND=mock`
- `VOICE_AGENT_TTS_BACKEND=mock`

That is enough to validate orchestration and audio return, but the TTS output is only a generated tone.

To use real HTTP modules, configure environment variables for the service. Recommended approaches:

1. Add `Environment=` lines directly to `/etc/systemd/system/webrtc-client.service`
2. Preferably create a systemd drop-in override with `sudo systemctl edit webrtc-client`

Example override when the modules run on the same Ubuntu host:

```ini
[Service]
Environment=VOICE_AGENT_ENABLE=true
Environment=VOICE_AGENT_ASR_BACKEND=http
Environment=VOICE_AGENT_LLM_BACKEND=http
Environment=VOICE_AGENT_TTS_BACKEND=http
Environment=VOICE_AGENT_ASR_ENDPOINT=http://127.0.0.1:8091/transcribe
Environment=VOICE_AGENT_LLM_ENDPOINT=http://127.0.0.1:8092/respond
Environment=VOICE_AGENT_TTS_ENDPOINT=http://127.0.0.1:8093/synthesize
```

After changing the service:

```bash
sudo systemctl daemon-reload
sudo systemctl restart webrtc-client
sudo systemctl status webrtc-client
```

If your ASR / LLM / TTS modules run on your local PC instead of the Ubuntu host, replace `127.0.0.1` with an address the Ubuntu host can actually reach.

### 2.7 Install and enable Nginx

The provided Nginx config serves `web/` and proxies WebSocket signaling at `/ws`.

Install the config:

```bash
sudo install -m 0644 /tmp/webrtc_client.conf /etc/nginx/sites-available/webrtc_client.conf
```

If you copied it to a temporary path instead, install from that path into `/etc/nginx/sites-available/webrtc_client.conf`.

Enable the site:

```bash
sudo ln -s /etc/nginx/sites-available/webrtc_client.conf /etc/nginx/sites-enabled/webrtc_client.conf
sudo nginx -t
sudo systemctl reload nginx
sudo systemctl status nginx
```

### 2.8 Open required ports

Ensure the server allows:

- `80/tcp` for Nginx
- `5000/udp` for ESP32 audio

Optional later:

- `443/tcp` if you terminate TLS on the VM

## 3. Optional Browser Access for WebRTC Checks

Browser microphone capture requires a trusted HTTPS origin.

If you do not have a domain and TLS certificate yet, use a temporary HTTPS tunnel such as `ngrok`:

```bash
ngrok http http://127.0.0.1:80
```

Open the generated `https://...ngrok-free.app` URL on your local PC browser.

This is only needed for browser transport validation. It is not required to validate the server-side ASR -> LLM -> TTS chain.

## 4. Validation Checklist

Validate in this order so you isolate problems by layer.

### 4.1 Validate the Go server on the Ubuntu host

On the server:

```bash
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/api/voice/status
sudo systemctl status webrtc-client
sudo journalctl -u webrtc-client -n 50 --no-pager
sudo ss -ulpn | grep 5000
```

Expected results:

- `/healthz` returns `ok: true`
- `webrtc_enabled` and `voice_enabled` are `true`
- `voice_session_id` is present
- the service is active
- UDP `:5000` is listening

### 4.2 Validate direct voice turns without ESP32

This checks `LLM -> TTS` orchestration first.

On the server:

```bash
curl -X POST http://127.0.0.1:8080/api/voice/text-turn \
  -H 'Content-Type: application/json' \
  -d '{"text":"hello from deployment test","speak":false}'
```

Expected results:

- HTTP `200`
- JSON reply includes `input_text`, `reply_text`, `turn_id`
- `reply_text` is non-empty
- `output_audio_bytes` is greater than `0` when real or mock TTS is enabled

Then confirm status updated:

```bash
curl http://127.0.0.1:8080/api/voice/status
```

Expected results:

- `last_turn` is populated
- `history_messages` increased

### 4.3 Validate software-only audio turns without ESP32

This checks the full server-side chain without hardware:

- `base64 PCMU audio -> Go server -> ASR -> LLM -> TTS -> Go server`

Use this before ESP32 debugging when ASR / LLM / TTS run locally on your PC and the Go server is either local or on the cloud.

First generate a known PCMU payload. One simple option is to call the local adapter `/synthesize` endpoint and reuse its `audio_base64` response:

```powershell
$tts = Invoke-RestMethod `
  -Method Post `
  -Uri http://127.0.0.1:8094/synthesize `
  -ContentType 'application/json' `
  -Body '{"session_id":"software-test","text":"hello from software audio test","audio_format":{"encoding":"g711_ulaw","sample_rate_hz":8000,"channels":1}}'

Invoke-RestMethod `
  -Method Post `
  -Uri http://127.0.0.1:8080/api/voice/audio-turn `
  -ContentType 'application/json' `
  -Body (@{audio_base64=$tts.audio_base64; speak=$false} | ConvertTo-Json)
```

Expected results:

- HTTP `200`
- `source` is `audio`
- `input_text` is produced by real ASR
- `reply_text` is produced by real LLM
- `output_audio_bytes` is greater than `0`

For cloud validation with ASR / LLM / TTS still on your PC, replace the module endpoints in the Go service environment with PC-reachable tunnel or VPN URLs. Keep `speak:false` while ESP32 is offline.

### 4.4 Validate outbound audio back to ESP32

This checks whether synthesized PCMU audio can be pushed from the cloud server back to the board.

Requirements:

- ESP32 is powered on
- ESP32 is connected to Wi-Fi
- ESP32 has already sent at least one UDP packet to the server so the server knows the return endpoint

On the server:

```bash
curl -X POST http://127.0.0.1:8080/api/voice/text-turn \
  -H 'Content-Type: application/json' \
  -d '{"text":"play a response to the device","speak":true}'
```

Expected results:

- request returns `200`
- ESP32 speaker plays audio
- server logs show an ESP32 UDP endpoint was discovered

### 4.5 Validate buffered ESP32 microphone audio

This checks the full current voice path:

- `ESP32 mic -> Cloud -> ASR -> LLM -> TTS -> Cloud -> ESP32 speaker`

First, let the ESP32 run long enough to send microphone audio. Then inspect status:

```bash
curl http://127.0.0.1:8080/api/voice/status
```

Expected results:

- `buffered_audio_bytes` is greater than `0`
- `esp32_endpoint` is populated

Then commit the buffered audio turn:

```bash
curl -X POST http://127.0.0.1:8080/api/voice/commit \
  -H 'Content-Type: application/json' \
  -d '{"speak":true}'
```

Expected results:

- HTTP `200`
- response contains `input_text` from ASR and `reply_text` from LLM
- `output_audio_bytes` is greater than `0`
- ESP32 speaker plays the response

If you use mock backends:

- ASR returns synthetic text
- LLM returns a simple echoed reply
- TTS returns a tone, not spoken speech

### 4.6 Validate browser transport separately

Only do this when checking WebRTC transport regressions.

Steps:

1. Open the HTTPS tunnel URL in a browser on your local PC.
2. Allow microphone access.
3. Confirm the browser connects and stays connected.
4. Confirm ESP32 audio reaches the browser.
5. Confirm browser-originated audio can still be observed by the ESP32 if you are testing the transport path.

## 5. Troubleshooting

If `/healthz` fails:

- check `sudo systemctl status webrtc-client`
- check `sudo journalctl -u webrtc-client -f`
- confirm the binary exists at `/opt/webrtc-client/bin/webrtc_server`

If WebRTC works but voice turns fail:

- check `/api/voice/status`
- verify `VOICE_AGENT_*` environment variables
- confirm ASR / LLM / TTS endpoints are reachable from the Ubuntu host

If `speak:true` succeeds but the ESP32 plays nothing:

- confirm `esp32_endpoint` is set in `/api/voice/status`
- confirm the ESP32 has already sent UDP traffic to the server
- confirm `5000/udp` is open in the firewall

If the ESP32 never reaches the server:

- re-check `UDP_SERVER_IP` in `mcu/src/main.c`
- confirm the server public IP is correct
- confirm the Wi-Fi credentials in firmware are correct

If browser microphone access fails:

- do not use raw `http://<server-ip>`
- use HTTPS through a domain or `ngrok`

## 6. Current Non-Goals

These are not part of the current deployment target:

- automatic end-of-utterance detection
- autonomous MCP/tool execution in the LLM loop
- TURN server deployment
- authentication
- multi-session routing

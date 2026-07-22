from __future__ import annotations

import argparse
import base64
import binascii
import contextlib
import io
import json
import math
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any


AUDIO_ENCODING = "g711_ulaw"
AUDIO_SAMPLE_RATE_HZ = 8000
AUDIO_CHANNELS = 1


def decode_ulaw_byte(ulaw_byte: int) -> int:
    ulaw_byte = (~ulaw_byte) & 0xFF
    sign = ulaw_byte & 0x80
    exponent = (ulaw_byte >> 4) & 0x07
    mantissa = ulaw_byte & 0x0F
    sample = ((mantissa << 3) + 0x84) << exponent
    sample -= 0x84
    if sign:
        sample = -sample
    return max(-32768, min(32767, sample))


def encode_ulaw_sample(sample: int) -> int:
    ulaw_bias = 0x84
    ulaw_clip = 32635

    sign = 0
    pcm = int(sample)
    if pcm < 0:
        sign = 0x80
        pcm = -pcm
    pcm = min(pcm, ulaw_clip)
    pcm += ulaw_bias

    exponent = 7
    mask = 0x4000
    while exponent > 0 and (pcm & mask) == 0:
        exponent -= 1
        mask >>= 1

    mantissa = (pcm >> (exponent + 3)) & 0x0F
    ulaw_byte = (~(sign | (exponent << 4) | mantissa)) & 0xFF
    if ulaw_byte == 0:
        ulaw_byte = 0x02
    return ulaw_byte


def ulaw_to_pcm16(ulaw: bytes) -> bytes:
    out = bytearray(len(ulaw) * 2)
    offset = 0
    for item in ulaw:
        sample = decode_ulaw_byte(item)
        out[offset : offset + 2] = int(sample).to_bytes(2, byteorder="little", signed=True)
        offset += 2
    return bytes(out)


def pcm16_to_ulaw(pcm16: bytes) -> bytes:
    if len(pcm16) % 2 != 0:
        raise ValueError("PCM16 byte length must be even")

    out = bytearray(len(pcm16) // 2)
    for index in range(0, len(pcm16), 2):
        sample = int.from_bytes(pcm16[index : index + 2], byteorder="little", signed=True)
        out[index // 2] = encode_ulaw_sample(sample)
    return bytes(out)


def generate_test_pcmu(text: str, duration_ms: int = 700) -> bytes:
    total_samples = max(1, AUDIO_SAMPLE_RATE_HZ * duration_ms // 1000)
    amplitude = 10000.0
    frequency = 440.0 + min(220.0, len(text.strip()) * 5.0)
    out = bytearray(total_samples)
    for index in range(total_samples):
        envelope = min(1.0, index / max(1.0, AUDIO_SAMPLE_RATE_HZ / 40.0))
        sample = int(math.sin(2 * math.pi * frequency * index / AUDIO_SAMPLE_RATE_HZ) * amplitude * envelope)
        out[index] = encode_ulaw_sample(sample)
    return bytes(out)


class VoiceAdapter:
    def __init__(
        self,
        client_dir: Path,
        preload: bool,
        asr_model_size: str | None,
        asr_language: str | None,
        tts_model_name: str | None,
        tts_speaker: str | None,
        tts_max_text_len: int | None,
        tts_device: str,
    ):
        self.client_dir = client_dir
        if str(client_dir) not in sys.path:
            sys.path.insert(0, str(client_dir))

        from config import (  # type: ignore
            SERVER_TTS_MAX_TEXT_LEN,
            TTS_MODEL_NAME,
            TTS_SPEAKER,
            WHISPER_LANGUAGE,
            WHISPER_MODEL_SIZE,
        )
        from speech_asr import WhisperAsrEngine  # type: ignore

        self.asr_model_size = asr_model_size or WHISPER_MODEL_SIZE
        self.asr_language = WHISPER_LANGUAGE if asr_language is None else asr_language or None
        self.tts_model_name = tts_model_name or TTS_MODEL_NAME
        self.tts_speaker = TTS_SPEAKER if tts_speaker is None else tts_speaker or None
        self.tts_max_text_len = int(tts_max_text_len or SERVER_TTS_MAX_TEXT_LEN)
        self.tts_device = normalize_tts_device(tts_device)

        self.asr = WhisperAsrEngine(model_size=self.asr_model_size, language=self.asr_language)
        self.tts = AdapterCoquiTtsEngine(
            model_name=self.tts_model_name,
            speaker=self.tts_speaker,
            device=self.tts_device,
        )
        self.asr_lock = threading.Lock()
        self.tts_lock = threading.Lock()
        self.started_at = time.time()
        self.asr_preloaded = False
        self.tts_preloaded = False

        if preload:
            self.preload()

    @staticmethod
    def _audio_format_error(audio_format: Any) -> str:
        if not isinstance(audio_format, dict):
            return "audio_format must be an object"
        if audio_format.get("encoding") != AUDIO_ENCODING:
            return f"audio_format.encoding must be {AUDIO_ENCODING}"
        try:
            sample_rate_hz = int(audio_format.get("sample_rate_hz") or 0)
            channels = int(audio_format.get("channels") or 0)
        except (TypeError, ValueError):
            return "audio_format.sample_rate_hz and channels must be integers"
        if sample_rate_hz != AUDIO_SAMPLE_RATE_HZ:
            return f"audio_format.sample_rate_hz must be {AUDIO_SAMPLE_RATE_HZ}"
        if channels != AUDIO_CHANNELS:
            return f"audio_format.channels must be {AUDIO_CHANNELS}"
        return ""

    def preload(self):
        with self.asr_lock:
            self.asr.preload()
            self.asr_preloaded = True
        with self.tts_lock:
            self.tts.preload()
            self.tts_preloaded = True

    def health(self) -> dict[str, Any]:
        return {
            "ok": True,
            "service": "local_voice_adapter",
            "uptime_sec": int(time.time() - self.started_at),
            "audio_format": {
                "encoding": AUDIO_ENCODING,
                "sample_rate_hz": AUDIO_SAMPLE_RATE_HZ,
                "channels": AUDIO_CHANNELS,
            },
            "asr": {
                "model_size": self.asr_model_size,
                "language": self.asr_language,
                "preloaded": self.asr_preloaded,
            },
            "tts": {
                "model_name": self.tts_model_name,
                "speaker": self.tts_speaker,
                "device": self.tts_device,
                "target_sample_rate_hz": AUDIO_SAMPLE_RATE_HZ,
                "preloaded": self.tts_preloaded,
            },
        }

    def transcribe(self, payload: dict[str, Any]) -> tuple[int, dict[str, Any]]:
        format_error = self._audio_format_error(payload.get("audio_format"))
        if format_error:
            return 400, {"error": format_error}

        audio_base64 = str(payload.get("audio_base64") or "")
        if not audio_base64:
            return 400, {"error": "audio_base64 is required"}

        try:
            pcmu = base64.b64decode(audio_base64, validate=True)
        except (binascii.Error, ValueError) as exc:
            return 400, {"error": f"invalid audio_base64: {exc}"}

        pcm16 = ulaw_to_pcm16(pcmu)
        with self.asr_lock:
            result = self.asr.transcribe_pcm16(
                pcm16=pcm16,
                sample_rate=AUDIO_SAMPLE_RATE_HZ,
                channels=AUDIO_CHANNELS,
                sample_width_bytes=2,
            )
        self.asr_preloaded = True

        if not result.ok:
            return 502, {"error": result.error or "ASR failed", "meta": {"latency_ms": result.latency_ms}}

        return 200, {
            "text": result.text,
            "final": True,
            "meta": {
                "backend": "local_whisper",
                "latency_ms": result.latency_ms,
                "audio_ms": result.audio_ms,
                "input_audio_bytes": len(pcmu),
            },
        }

    def synthesize(self, payload: dict[str, Any]) -> tuple[int, dict[str, Any]]:
        text = str(payload.get("text") or "").strip()
        if not text:
            return 400, {"error": "text is required"}
        if len(text) > self.tts_max_text_len:
            return 400, {"error": f"text exceeds max length {self.tts_max_text_len}"}

        format_error = self._audio_format_error(payload.get("audio_format"))
        if format_error:
            return 400, {"error": format_error}

        with self.tts_lock:
            result = self.tts.synthesize_pcm16(text=text, target_sample_rate=AUDIO_SAMPLE_RATE_HZ)
        self.tts_preloaded = True

        if not result.ok:
            return 502, {"error": result.error or "TTS failed", "meta": {"latency_ms": result.latency_ms}}
        if result.sample_rate != AUDIO_SAMPLE_RATE_HZ:
            return 502, {"error": f"TTS returned unexpected sample_rate {result.sample_rate}"}

        pcmu = pcm16_to_ulaw(result.pcm16)
        return 200, {
            "audio_base64": base64.b64encode(pcmu).decode("ascii"),
            "meta": {
                "backend": "local_coqui_tts",
                "latency_ms": result.latency_ms,
                "output_audio_bytes": len(pcmu),
                "sample_rate_hz": AUDIO_SAMPLE_RATE_HZ,
            },
        }


class AdapterCoquiTtsEngine:
    def __init__(self, model_name: str, speaker: str | None, device: str):
        self._model_name = model_name
        self._speaker = speaker
        self._device = device
        self._engine = None
        self._lock = threading.Lock()

    def _get_engine(self):
        if self._engine is not None:
            return self._engine

        with self._lock:
            if self._engine is not None:
                return self._engine
            try:
                from TTS.api import TTS  # type: ignore[reportMissingImports]
            except Exception as exc:
                raise RuntimeError(f"TTS import failed: {exc}") from exc

            with contextlib.redirect_stdout(io.StringIO()):
                engine = TTS(
                    model_name=self._model_name,
                    progress_bar=False,
                    gpu=self._device == "cuda",
                )
                if hasattr(engine, "to"):
                    engine.to(self._device)
            self._engine = engine
            return self._engine

    def preload(self):
        self._get_engine()

    @staticmethod
    def _resample_if_needed(wav: Any, src_rate: int, dst_rate: int):
        import numpy as np

        wav_np = np.array(wav, dtype=np.float32)
        if src_rate == dst_rate or wav_np.size == 0:
            return wav_np

        duration = wav_np.size / float(src_rate)
        dst_len = max(1, int(duration * dst_rate))
        x_src = np.linspace(0.0, 1.0, wav_np.size, endpoint=False)
        x_dst = np.linspace(0.0, 1.0, dst_len, endpoint=False)
        return np.interp(x_dst, x_src, wav_np).astype(np.float32)

    def synthesize_pcm16(self, text: str, target_sample_rate: int):
        from speech_types import TtsResult  # type: ignore
        import numpy as np

        started = time.perf_counter()
        if not text.strip():
            return TtsResult(ok=False, pcm16=b"", sample_rate=target_sample_rate, latency_ms=0, error="empty_text")

        try:
            engine = self._get_engine()
        except Exception as exc:
            latency_ms = int((time.perf_counter() - started) * 1000)
            return TtsResult(ok=False, pcm16=b"", sample_rate=target_sample_rate, latency_ms=latency_ms, error=str(exc))

        try:
            with contextlib.redirect_stdout(io.StringIO()):
                if self._speaker:
                    wav = engine.tts(text=text, speaker=self._speaker)
                else:
                    wav = engine.tts(text=text)

            src_rate = int(getattr(engine.synthesizer, "output_sample_rate", target_sample_rate))
            wav_np = self._resample_if_needed(wav, src_rate=src_rate, dst_rate=target_sample_rate)
            pcm16 = np.clip(wav_np * 32767.0, -32768.0, 32767.0).astype(np.int16).tobytes()
            latency_ms = int((time.perf_counter() - started) * 1000)
            return TtsResult(ok=True, pcm16=pcm16, sample_rate=target_sample_rate, latency_ms=latency_ms)
        except Exception as exc:
            latency_ms = int((time.perf_counter() - started) * 1000)
            return TtsResult(ok=False, pcm16=b"", sample_rate=target_sample_rate, latency_ms=latency_ms, error=str(exc))


def env_or_default(name: str, fallback: str) -> str:
    value = os.environ.get(name)
    return value if value not in (None, "") else fallback


def env_bool_or_default(name: str, fallback: bool) -> bool:
    value = os.environ.get(name)
    if value in (None, ""):
        return fallback
    return value.strip().lower() in {"1", "true", "yes", "on"}


def env_int_or_default(name: str, fallback: int) -> int:
    value = os.environ.get(name)
    if value in (None, ""):
        return fallback
    try:
        return int(value)
    except ValueError:
        return fallback


def normalize_tts_device(device: str) -> str:
    normalized = (device or "auto").strip().lower()
    if normalized == "auto":
        try:
            import torch  # type: ignore

            return "cuda" if torch.cuda.is_available() else "cpu"
        except Exception:
            return "cpu"
    if normalized not in {"cpu", "cuda"}:
        raise ValueError("tts_device must be cpu, cuda, or auto")
    return normalized


def make_handler(adapter: VoiceAdapter):
    class Handler(BaseHTTPRequestHandler):
        server_version = "LocalVoiceAdapter/1.0"

        def do_GET(self):
            if self.path == "/healthz":
                self.write_json(200, adapter.health())
                return
            self.write_json(404, {"error": "not found"})

        def do_POST(self):
            try:
                payload = self.read_json()
            except ValueError as exc:
                self.write_json(400, {"error": str(exc)})
                return

            if self.path == "/transcribe":
                status, response = adapter.transcribe(payload)
            elif self.path == "/synthesize":
                status, response = adapter.synthesize(payload)
            elif self.path == "/debug/test-tone":
                text = str(payload.get("text") or "test")
                pcmu = generate_test_pcmu(text)
                status, response = 200, {
                    "audio_base64": base64.b64encode(pcmu).decode("ascii"),
                    "meta": {"backend": "adapter_test_tone", "output_audio_bytes": len(pcmu)},
                }
            else:
                status, response = 404, {"error": "not found"}

            self.write_json(status, response)

        def read_json(self) -> dict[str, Any]:
            content_length = int(self.headers.get("Content-Length") or 0)
            if content_length <= 0:
                return {}
            raw = self.rfile.read(content_length)
            try:
                loaded = json.loads(raw.decode("utf-8"))
            except json.JSONDecodeError as exc:
                raise ValueError(f"invalid JSON: {exc}") from exc
            if not isinstance(loaded, dict):
                raise ValueError("JSON body must be an object")
            return loaded

        def write_json(self, status: int, payload: dict[str, Any]):
            body = json.dumps(payload).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, format: str, *args: Any):
            sys.stderr.write("%s - %s\n" % (self.log_date_time_string(), format % args))

    return Handler


def parse_args() -> argparse.Namespace:
    repo_root = Path(__file__).resolve().parents[1]
    default_client_dir = repo_root.parent / "client"

    parser = argparse.ArgumentParser(description="HTTP adapter for local ASR/TTS modules")
    parser.add_argument("--host", default=env_or_default("VOICE_ADAPTER_HOST", "127.0.0.1"), help="Host to bind")
    parser.add_argument("--port", default=env_int_or_default("VOICE_ADAPTER_PORT", 8094), type=int, help="Port to bind")
    parser.add_argument("--client-dir", default=env_or_default("VOICE_ADAPTER_CLIENT_DIR", str(default_client_dir)), help="Path to the local client repo")
    parser.add_argument("--preload", action="store_true", default=env_bool_or_default("VOICE_ADAPTER_PRELOAD", False), help="Load ASR and TTS models before serving")
    parser.add_argument("--asr-model-size", default=os.environ.get("VOICE_ADAPTER_ASR_MODEL_SIZE"), help="Override client WHISPER_MODEL_SIZE")
    parser.add_argument("--asr-language", default=os.environ.get("VOICE_ADAPTER_ASR_LANGUAGE"), help="Override client WHISPER_LANGUAGE; use empty string for auto-detect")
    parser.add_argument("--tts-model-name", default=os.environ.get("VOICE_ADAPTER_TTS_MODEL_NAME"), help="Override client TTS_MODEL_NAME")
    parser.add_argument("--tts-speaker", default=os.environ.get("VOICE_ADAPTER_TTS_SPEAKER"), help="Override client TTS_SPEAKER; use empty string for no speaker")
    parser.add_argument("--tts-max-text-len", default=env_int_or_default("VOICE_ADAPTER_TTS_MAX_TEXT_LEN", 0), type=int, help="Override max text length")
    parser.add_argument("--tts-device", default=env_or_default("VOICE_ADAPTER_TTS_DEVICE", "auto"), choices=["auto", "cpu", "cuda"], help="TTS runtime device")
    return parser.parse_args()


def main():
    args = parse_args()
    client_dir = Path(args.client_dir).resolve()
    if not client_dir.exists():
        raise SystemExit(f"client-dir does not exist: {client_dir}")

    adapter = VoiceAdapter(
        client_dir=client_dir,
        preload=args.preload,
        asr_model_size=args.asr_model_size,
        asr_language=args.asr_language,
        tts_model_name=args.tts_model_name,
        tts_speaker=args.tts_speaker,
        tts_max_text_len=args.tts_max_text_len if args.tts_max_text_len > 0 else None,
        tts_device=args.tts_device,
    )
    server = ThreadingHTTPServer((args.host, args.port), make_handler(adapter))
    print(f"local voice adapter listening on http://{args.host}:{args.port}", flush=True)
    print(f"client-dir: {client_dir}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()

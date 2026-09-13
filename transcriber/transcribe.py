"""faster-whisper transcription on CPU, rendered as Markdown with timecodes."""

from __future__ import annotations

import os
import sys
from dataclasses import dataclass

# Loaded models are expensive (large-v3 ~3GB); keep one per configuration per process.
_MODELS: dict[tuple[str, str, str], object] = {}


@dataclass
class Segment:
    start: float  # seconds
    end: float
    text: str


def _get_model(model: str, device: str, compute_type: str):
    key = (model, device, compute_type)
    if key not in _MODELS:
        # Lazy import + lazy construction: importing this module must not touch the model cache
        # (HF_HOME) or pull faster-whisper's heavy deps.
        from faster_whisper import WhisperModel

        _MODELS[key] = WhisperModel(model, device=device, compute_type=compute_type)
    return _MODELS[key]


def transcribe(
    audio_path: str, *, model: str | None = None, language: str | None = None
) -> list[Segment]:
    """Transcribe an audio file (WebM/Opus is fine — faster-whisper decodes via PyAV).

    Config falls back to the environment: WHISPER_MODEL (large-v3), WHISPER_LANGUAGE
    (unset = autodetect), WHISPER_DEVICE (cpu), WHISPER_COMPUTE_TYPE (int8).
    """
    model = model or os.getenv("WHISPER_MODEL") or "large-v3"
    language = language or os.getenv("WHISPER_LANGUAGE") or None
    device = os.getenv("WHISPER_DEVICE") or "cpu"
    compute_type = os.getenv("WHISPER_COMPUTE_TYPE") or "int8"

    whisper = _get_model(model, device, compute_type)
    segments, _info = whisper.transcribe(
        audio_path, language=language, vad_filter=True, beam_size=5
    )
    return [Segment(s.start, s.end, s.text) for s in segments]


def _timecode(seconds: float) -> str:
    hours, rest = divmod(int(seconds), 3600)
    minutes, secs = divmod(rest, 60)
    return f"{hours}:{minutes:02d}:{secs:02d}" if hours else f"{minutes:02d}:{secs:02d}"


def to_markdown(segments: list[Segment]) -> str:
    """One '[MM:SS] text' line per segment ('[H:MM:SS]' from an hour in); blanks dropped."""
    return "\n".join(
        f"[{_timecode(s.start)}] {s.text.strip()}" for s in segments if s.text.strip()
    )


def main(argv: list[str] | None = None) -> int:
    args = sys.argv[1:] if argv is None else argv
    if len(args) != 1:
        print("usage: python -m transcriber.transcribe <audio>", file=sys.stderr)
        return 2
    print(to_markdown(transcribe(args[0])))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

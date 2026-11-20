from __future__ import annotations

import io
import subprocess
import sys
import tempfile
import wave

from qol_processor import (
    OUTPUT_SAMPLE_RATE,
    claim_stdout,
    clamp,
    loudness_gain,
    read_synthesis_requests,
    speaking_speed,
    write_pcm,
)

BASE_WORDS_PER_MINUTE = 150
BASE_PITCH = 50
BASE_AMPLITUDE = 100


def voice_arguments(prosody, pace: float = 1.0) -> list[str]:
    rate = BASE_WORDS_PER_MINUTE * speaking_speed(prosody, 0.6, 1.8) * (pace or 1.0)
    pitch = BASE_PITCH
    amplitude = BASE_AMPLITUDE * loudness_gain(prosody)
    if prosody is not None:
        pitch += clamp(prosody.relative_pitch_semitones * 4, -30, 30)
    return ["-s", str(round(rate)), "-p", str(round(pitch)), "-a", str(round(clamp(amplitude, 20, 200)))]


def wav_pcm(wav: bytes) -> bytes:
    with wave.open(io.BytesIO(wav), "rb") as reader:
        if reader.getnchannels() != 1 or reader.getsampwidth() != 2 or reader.getframerate() != OUTPUT_SAMPLE_RATE:
            raise RuntimeError("Spanish TTS must emit 22050 Hz mono S16LE")
        pcm = reader.readframes(reader.getnframes())
    if not pcm:
        raise RuntimeError("Spanish TTS produced no audio")
    return pcm


def synthesize(text: str, prosody, pace: float) -> bytes:
    with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8") as source:
        source.write(text)
        source.flush()
        command = ["espeak", "-v", "es", *voice_arguments(prosody, pace), "-f", source.name, "--stdout"]
        wav = subprocess.run(command, check=True, stdout=subprocess.PIPE).stdout
    return wav_pcm(wav)


def main() -> None:
    output = claim_stdout()
    for request in read_synthesis_requests(sys.stdin.buffer):
        if request.language != "es":
            raise RuntimeError(f"espeak processor only synthesizes Spanish, got {request.language!r}")
        prosody = request.prosody if request.HasField("prosody") else None
        write_pcm(output, synthesize(request.text, prosody, request.pace))


if __name__ == "__main__":
    main()

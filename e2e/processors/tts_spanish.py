from __future__ import annotations

import io
import subprocess
import sys
import tempfile
import wave


def wav_pcm(wav: bytes) -> bytes:
    with wave.open(io.BytesIO(wav), "rb") as reader:
        if reader.getnchannels() != 1 or reader.getsampwidth() != 2 or reader.getframerate() != 22050:
            raise RuntimeError("Spanish TTS must emit 22050 Hz mono S16LE")
        pcm = reader.readframes(reader.getnframes())
    if not pcm:
        raise RuntimeError("Spanish TTS produced no audio")
    return pcm


def main() -> None:
    text = sys.stdin.read().strip()
    if not text:
        return
    with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8") as source:
        source.write(text)
        source.flush()
        command = [
            "espeak",
            "-v",
            "es",
            "-s",
            "150",
            "-f",
            source.name,
            "--stdout",
        ]
        wav = subprocess.run(command, check=True, stdout=subprocess.PIPE).stdout
    sys.stdout.buffer.write(wav_pcm(wav))


if __name__ == "__main__":
    main()

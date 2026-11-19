from __future__ import annotations

import os
import sys
import time

from qol_processor import (
    OUTPUT_SAMPLE_RATE,
    claim_stdout,
    loudness_gain,
    read_synthesis_requests,
    speaking_speed,
    write_pcm,
)

KOKORO_SAMPLE_RATE = 24000


def main() -> None:
    output = claim_stdout()
    import numpy as np
    import torch
    from kokoro import KPipeline
    from scipy.signal import resample_poly

    device = os.environ.get("QOL_KOKORO_DEVICE") or ("cuda" if torch.cuda.is_available() else "cpu")
    voice = os.environ.get("QOL_KOKORO_VOICE", "em_alex")
    pipeline = KPipeline(lang_code="e", repo_id=os.environ.get("QOL_KOKORO_MODEL", "hexgrad/Kokoro-82M"), device=device)
    print(f"kokoro ready device={device} voice={voice}", file=sys.stderr, flush=True)
    for request in read_synthesis_requests(sys.stdin.buffer):
        if request.language != "es":
            raise RuntimeError(f"Kokoro processor is configured for Spanish, got {request.language!r}")
        prosody = request.prosody if request.HasField("prosody") else None
        speed = speaking_speed(prosody)
        gain = loudness_gain(prosody)
        started = time.monotonic()
        produced = 0
        for result in pipeline(request.text, voice=voice, speed=speed):
            if result.audio is None:
                continue
            audio = np.asarray(result.audio, dtype=np.float32) * gain
            audio = resample_poly(audio, OUTPUT_SAMPLE_RATE // 150, KOKORO_SAMPLE_RATE // 150)
            pcm = (np.clip(audio, -1.0, 1.0) * 32767.0).round().astype("<i2").tobytes()
            produced += len(pcm)
            write_pcm(output, pcm, end=False)
        if produced == 0:
            raise RuntimeError("Kokoro produced no audio")
        write_pcm(output, b"", end=True)
        print(
            f"kokoro synthesized {produced / 2 / OUTPUT_SAMPLE_RATE:.2f}s in {time.monotonic() - started:.3f}s speed={speed:.2f} gain={gain:.2f}",
            file=sys.stderr,
            flush=True,
        )


if __name__ == "__main__":
    main()

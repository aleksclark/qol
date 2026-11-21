from __future__ import annotations

import os
import sys
import time

from qol_processor import (
    OUTPUT_SAMPLE_RATE,
    claim_stdout,
    clamp,
    loudness_gain,
    read_synthesis_requests,
    speaking_speed,
    write_pcm,
)

KOKORO_SAMPLE_RATE = 24000
VOICED_HZ = 40.0


class PitchControl:
    def __init__(self, predictor) -> None:
        self.original = predictor.F0Ntrain
        self.shift = 0.0
        self.range_delta = 0.0
        predictor.F0Ntrain = self

    def __call__(self, features, style):
        import math

        import torch

        f0, energy = self.original(features, style)
        if self.shift == 0 and self.range_delta == 0:
            return f0, energy
        voiced = f0 > VOICED_HZ
        if int(voiced.sum()) < 3:
            return f0, energy
        log_f0 = torch.log(f0.clamp(min=1.0))
        voiced_log = log_f0[voiced].float()
        center = voiced_log.median()
        semitone = math.log(2) / 12
        current = float((torch.quantile(voiced_log, 0.9) - torch.quantile(voiced_log, 0.1)) / semitone)
        scale = clamp((current + self.range_delta) / current, 0.5, 2.0) if current > 0.5 else 1.0
        adjusted = center + (log_f0 - center) * scale + self.shift * semitone
        return torch.where(voiced, torch.exp(adjusted).to(f0.dtype), f0), energy

    def configure(self, prosody) -> None:
        if prosody is None:
            self.shift, self.range_delta = 0.0, 0.0
            return
        self.shift = clamp(prosody.relative_pitch_semitones * 0.75, -4.0, 4.0)
        self.range_delta = clamp(prosody.relative_pitch_range_semitones, -4.0, 6.0)


def main() -> None:
    output = claim_stdout()
    import numpy as np
    import torch
    from kokoro import KPipeline
    from scipy.signal import resample_poly

    device = os.environ.get("QOL_KOKORO_DEVICE") or ("cuda" if torch.cuda.is_available() else "cpu")
    voice = os.environ.get("QOL_KOKORO_VOICE", "em_alex")
    pipeline = KPipeline(lang_code="e", repo_id=os.environ.get("QOL_KOKORO_MODEL", "hexgrad/Kokoro-82M"), device=device)
    pitch = PitchControl(pipeline.model.predictor)
    print(f"kokoro ready device={device} voice={voice}", file=sys.stderr, flush=True)
    for request in read_synthesis_requests(sys.stdin.buffer):
        if request.language != "es":
            raise RuntimeError(f"Kokoro processor is configured for Spanish, got {request.language!r}")
        prosody = request.prosody if request.HasField("prosody") else None
        speed = clamp(speaking_speed(prosody) * (request.pace or 1.0), 0.7, 1.8)
        gain = loudness_gain(prosody)
        pitch.configure(prosody)
        started = time.monotonic()
        segments = [np.asarray(result.audio, dtype=np.float32) for result in pipeline(request.text, voice=voice, speed=speed) if result.audio is not None]
        if not segments:
            raise RuntimeError("Kokoro produced no audio")
        audio = resample_poly(np.concatenate(segments) * gain, OUTPUT_SAMPLE_RATE // 150, KOKORO_SAMPLE_RATE // 150)
        pcm = (np.clip(audio, -1.0, 1.0) * 32767.0).round().astype("<i2").tobytes()
        produced = len(pcm)
        write_pcm(output, pcm, end=True)
        print(
            f"kokoro synthesized {produced / 2 / OUTPUT_SAMPLE_RATE:.2f}s in {time.monotonic() - started:.3f}s speed={speed:.2f} gain={gain:.2f} shift={pitch.shift:+.1f}st range={pitch.range_delta:+.1f}st",
            file=sys.stderr,
            flush=True,
        )


if __name__ == "__main__":
    main()

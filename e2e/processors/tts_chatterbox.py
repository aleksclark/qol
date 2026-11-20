from __future__ import annotations

import os
import sys
import tempfile
import time
import wave

from qol_processor import (
    OUTPUT_SAMPLE_RATE,
    claim_stdout,
    clamp,
    loudness_gain,
    read_synthesis_requests,
    write_pcm,
)


def expressiveness(prosody) -> tuple[float, float]:
    if prosody is None:
        return 0.5, 0.5
    exaggeration = clamp(0.25 + 0.75 * prosody.arousal, 0.25, 1.0)
    rate = prosody.relative_rate or 1.0
    cfg_weight = clamp(0.5 - (exaggeration - 0.5) * 0.4 - (1.0 - rate) * 0.5, 0.2, 0.7)
    return exaggeration, cfg_weight


def reference_path(prosody, directory: str):
    if prosody is None or not prosody.HasField("reference_audio") or os.environ.get("QOL_CHATTERBOX_USE_REFERENCE", "1") != "1":
        return None
    audio = prosody.reference_audio
    if audio.format.channel_count != 1 or not audio.data:
        return None
    path = os.path.join(directory, "reference.wav")
    with wave.open(path, "wb") as writer:
        writer.setnchannels(1)
        writer.setsampwidth(2)
        writer.setframerate(audio.format.sample_rate_hz)
        writer.writeframes(audio.data)
    return path


def main() -> None:
    output = claim_stdout()
    import numpy as np
    import torch
    from chatterbox.mtl_tts import ChatterboxMultilingualTTS
    from scipy.signal import resample_poly

    device = os.environ.get("QOL_CHATTERBOX_DEVICE") or ("cuda" if torch.cuda.is_available() else "cpu")
    model = ChatterboxMultilingualTTS.from_pretrained(device=device)
    reference = os.environ.get("QOL_CHATTERBOX_VOICE") or None
    print(f"chatterbox ready device={device} sr={model.sr}", file=sys.stderr, flush=True)
    scratch = tempfile.mkdtemp(prefix="qol-chatterbox-")
    for request in read_synthesis_requests(sys.stdin.buffer):
        prosody = request.prosody if request.HasField("prosody") else None
        exaggeration, cfg_weight = expressiveness(prosody)
        prompt = reference_path(prosody, scratch) or reference
        started = time.monotonic()
        output_wave = model.generate(
            request.text,
            language_id=request.language,
            audio_prompt_path=prompt,
            exaggeration=exaggeration,
            cfg_weight=cfg_weight,
        )
        audio = output_wave.squeeze(0).detach().cpu().numpy().astype(np.float32) * loudness_gain(prosody)
        audio = resample_poly(audio, OUTPUT_SAMPLE_RATE // 50, model.sr // 50)
        pcm = (np.clip(audio, -1.0, 1.0) * 32767.0).round().astype("<i2").tobytes()
        write_pcm(output, pcm)
        print(
            f"chatterbox synthesized {len(pcm) / 2 / OUTPUT_SAMPLE_RATE:.2f}s in {time.monotonic() - started:.2f}s "
            f"exaggeration={exaggeration:.2f} cfg={cfg_weight:.2f} reference={'source' if prompt and prompt != reference else 'default'}",
            file=sys.stderr,
            flush=True,
        )


if __name__ == "__main__":
    main()

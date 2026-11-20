from __future__ import annotations

import os
import sys
import time

from qol_processor import (
    OUTPUT_SAMPLE_RATE,
    claim_stdout,
    describe_delivery,
    read_synthesis_requests,
    write_pcm,
)

LANGUAGES = {"es": "Spanish", "en": "English"}


def load_model():
    import torch
    from qwen_tts import Qwen3TTSModel

    model_id = os.environ.get("QOL_QWEN_TTS_MODEL", "Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice")
    if not torch.cuda.is_available():
        raise RuntimeError("Qwen3-TTS processor requires a CUDA GPU")
    dtype = torch.bfloat16 if torch.cuda.get_device_capability()[0] >= 8 else torch.float16
    return Qwen3TTSModel.from_pretrained(
        model_id,
        device_map=os.environ.get("QOL_QWEN_TTS_DEVICE", "cuda:0"),
        dtype=dtype,
        attn_implementation="sdpa",
    )


def to_pcm(wave, sample_rate: int) -> bytes:
    import numpy as np
    import torch
    import torchaudio.functional as audio_functional

    tensor = torch.from_numpy(np.asarray(wave, dtype=np.float32))
    if sample_rate != OUTPUT_SAMPLE_RATE:
        tensor = audio_functional.resample(tensor, sample_rate, OUTPUT_SAMPLE_RATE)
    clipped = torch.clamp(tensor, -1.0, 1.0)
    return (clipped * 32767.0).round().to(torch.int16).numpy().tobytes()


def main() -> None:
    output = claim_stdout()
    model = load_model()
    speakers = model.get_supported_speakers() or []
    speaker = os.environ.get("QOL_QWEN_TTS_SPEAKER", "")
    if not speaker:
        if not speakers:
            raise RuntimeError("QOL_QWEN_TTS_SPEAKER is required for this model")
        speaker = speakers[0]
    print(f"qwen3-tts ready speaker={speaker} speakers={speakers}", file=sys.stderr, flush=True)
    for request in read_synthesis_requests(sys.stdin.buffer):
        language = LANGUAGES.get(request.language)
        if language is None:
            raise RuntimeError(f"unsupported synthesis language {request.language!r}")
        instruct = describe_delivery(request.prosody if request.HasField("prosody") else None)
        started = time.monotonic()
        waves, sample_rate = model.generate_custom_voice(
            text=request.text,
            language=language,
            speaker=speaker,
            instruct=instruct,
        )
        pcm = to_pcm(waves[0], sample_rate)
        elapsed = time.monotonic() - started
        seconds = len(pcm) / 2 / OUTPUT_SAMPLE_RATE
        print(
            f"qwen3-tts synthesized {seconds:.2f}s in {elapsed:.2f}s instruct={instruct!r}",
            file=sys.stderr,
            flush=True,
        )
        write_pcm(output, pcm)


if __name__ == "__main__":
    main()

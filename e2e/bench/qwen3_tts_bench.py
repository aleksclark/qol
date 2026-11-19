from __future__ import annotations

import os
import time

import torch
from qwen_tts import Qwen3TTSModel

TEXT = "Hermanos, hoy quiero hablarles de la esperanza que nos sostiene en los momentos difíciles."
CASES = {
    "neutral": "Speak in a natural, neutral conversational tone.",
    "fast": "Speak at a very fast pace, with a loud, projecting voice, with lively, animated intonation, sounding excited and intense.",
    "slow": "Speak at a very slow, deliberate pace, with a soft, quiet voice, with flat, even intonation, sounding calm and relaxed.",
}


def main() -> None:
    models = os.environ.get(
        "QOL_BENCH_MODELS",
        "Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice,Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice",
    ).split(",")
    trials = int(os.environ.get("QOL_BENCH_TRIALS", "3"))
    dtype = torch.bfloat16 if torch.cuda.is_bf16_supported() else torch.float16
    print(f"gpu={torch.cuda.get_device_name(0)} dtype={dtype}", flush=True)
    for model_id in models:
        model = Qwen3TTSModel.from_pretrained(model_id, device_map="cuda:0", dtype=dtype, attn_implementation="sdpa")
        model.generate_custom_voice(text="Hola.", language="Spanish", speaker="ryan", instruct=CASES["neutral"])
        for name, instruct in CASES.items():
            for trial in range(trials):
                started = time.monotonic()
                waves, sample_rate = model.generate_custom_voice(text=TEXT, language="Spanish", speaker="ryan", instruct=instruct)
                torch.cuda.synchronize()
                wall = time.monotonic() - started
                seconds = len(waves[0]) / sample_rate
                print(
                    f"RESULT model={model_id.rsplit('/', 1)[-1]} case={name} trial={trial} "
                    f"audio={seconds:.2f}s wall={wall:.2f}s rtf={wall / seconds:.2f}",
                    flush=True,
                )
        del model
        torch.cuda.empty_cache()


if __name__ == "__main__":
    main()

from __future__ import annotations

import os
import sys
import time

from qol_processor import claim_stdout, processor_pb2, qol_pb2, read_frame, write_frame


def english_label(label: str) -> str:
    return label.split("/")[-1].strip().lower()


def main() -> None:
    output = claim_stdout()
    import numpy as np
    from funasr import AutoModel

    model = AutoModel(
        model=os.environ.get("QOL_EMOTION_MODEL", "emotion2vec/emotion2vec_plus_base"),
        hub="hf",
        device=os.environ.get("QOL_EMOTION_DEVICE", "cpu"),
        disable_update=True,
        disable_pbar=True,
        disable_log=True,
    )
    print("emotion2vec ready", file=sys.stderr, flush=True)
    while True:
        request = read_frame(sys.stdin.buffer, qol_pb2.AudioPayload())
        if request is None:
            return
        if request.format.sample_rate_hz != 16000 or request.format.channel_count != 1:
            raise RuntimeError("emotion2vec expects 16 kHz mono PCM")
        if request.format.sample_format != qol_pb2.PCM_SAMPLE_FORMAT_S16_LE:
            raise RuntimeError("emotion2vec expects S16LE PCM")
        started = time.monotonic()
        wave = np.frombuffer(request.data, dtype="<i2").astype(np.float32) / 32768.0
        result = model.generate(wave, granularity="utterance", extract_embedding=False, disable_pbar=True)[0]
        scores = {}
        for label, score in zip(result["labels"], result["scores"]):
            name = english_label(label)
            if name and name != "<unk>":
                scores[name] = max(0.0, min(1.0, float(score)))
        best = max(scores, key=scores.get)
        write_frame(
            output,
            processor_pb2.EmotionResult(
                label=best,
                confidence=scores[best],
                scores=[qol_pb2.EmotionScore(label=name, score=value) for name, value in sorted(scores.items(), key=lambda item: -item[1])],
            ),
        )
        output.flush()
        print(
            f"emotion2vec {len(wave) / 16000:.2f}s -> {best} {scores[best]:.2f} in {time.monotonic() - started:.3f}s",
            file=sys.stderr,
            flush=True,
        )


if __name__ == "__main__":
    main()

from __future__ import annotations

import glob
import os
import subprocess
import sys

import numpy as np
import sherpa_onnx

SAMPLE_RATE = 16000
FRAME_BYTES = 3200


def first_existing(directory: str, *patterns: str) -> str:
    for pattern in patterns:
        matches = sorted(glob.glob(os.path.join(directory, pattern)))
        if matches:
            return matches[0]
    raise FileNotFoundError(f"no files matching {patterns} in {directory}")


def model_paths() -> tuple[str, str, str, str]:
    model_dir = os.environ.get(
        "QOL_SHERPA_MODEL",
        "/models/sherpa-onnx-streaming-zipformer-en-2023-06-26",
    )
    encoder = os.environ.get("QOL_SHERPA_ENCODER") or first_existing(
        model_dir, "encoder*.int8.onnx", "encoder*.onnx"
    )
    decoder = os.environ.get("QOL_SHERPA_DECODER") or first_existing(
        model_dir, "decoder-*.onnx"
    )
    if decoder.endswith(".int8.onnx"):
        full = decoder[: -len(".int8.onnx")] + ".onnx"
        if os.path.isfile(full):
            decoder = full
    joiner = os.environ.get("QOL_SHERPA_JOINER") or first_existing(
        model_dir, "joiner*.int8.onnx", "joiner*.onnx"
    )
    tokens = os.environ.get("QOL_SHERPA_TOKENS") or os.path.join(model_dir, "tokens.txt")
    if not os.path.isfile(tokens):
        raise FileNotFoundError(tokens)
    return encoder, decoder, joiner, tokens


def emit(text: str) -> None:
    text = text.strip()
    if text:
        print(text, flush=True)


def main() -> None:
    encoder, decoder, joiner, tokens = model_paths()
    recognizer = sherpa_onnx.OnlineRecognizer.from_transducer(
        tokens=tokens,
        encoder=encoder,
        decoder=decoder,
        joiner=joiner,
        num_threads=2,
        sample_rate=SAMPLE_RATE,
        feature_dim=80,
        decoding_method="greedy_search",
        provider="cpu",
        enable_endpoint_detection=True,
        rule1_min_trailing_silence=1.2,
        rule2_min_trailing_silence=0.6,
        rule3_min_utterance_length=5.0,
    )
    ffmpeg = subprocess.Popen(
        [
            "ffmpeg",
            "-hide_banner",
            "-loglevel",
            "error",
            "-fflags",
            "+nobuffer",
            "-probesize",
            "32768",
            "-analyzeduration",
            "0",
            "-i",
            "pipe:0",
            "-f",
            "s16le",
            "-acodec",
            "pcm_s16le",
            "-ar",
            str(SAMPLE_RATE),
            "-ac",
            "1",
            "pipe:1",
        ],
        stdin=sys.stdin.buffer,
        stdout=subprocess.PIPE,
    )
    assert ffmpeg.stdout is not None
    stream = recognizer.create_stream()
    try:
        while True:
            data = ffmpeg.stdout.read(FRAME_BYTES)
            if not data:
                break
            samples = np.frombuffer(data, dtype=np.int16).astype(np.float32) / 32768.0
            stream.accept_waveform(SAMPLE_RATE, samples)
            while recognizer.is_ready(stream):
                recognizer.decode_stream(stream)
            if recognizer.is_endpoint(stream):
                emit(recognizer.get_result(stream))
                recognizer.reset(stream)
        tail = np.zeros(int(0.5 * SAMPLE_RATE), dtype=np.float32)
        stream.accept_waveform(SAMPLE_RATE, tail)
        stream.input_finished()
        while recognizer.is_ready(stream):
            recognizer.decode_stream(stream)
        emit(recognizer.get_result(stream))
    finally:
        if ffmpeg.poll() is None:
            ffmpeg.terminate()
        ffmpeg.wait()


if __name__ == "__main__":
    main()

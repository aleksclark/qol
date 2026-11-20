from __future__ import annotations

import glob
import os
import subprocess
import sys

import numpy as np
import sherpa_onnx
from qol_processor import claim_stdout, write_utterance

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


def sample_ns(samples: int) -> int:
    return samples * 1_000_000_000 // SAMPLE_RATE


def seconds_ns(seconds: float) -> int:
    return round(seconds * 1_000_000_000)


def normalize(text: str) -> str:
    words = text.strip().lower().split()
    words = ["I" if word == "i" else "I" + word[1:] if word.startswith("i'") else word for word in words]
    sentence = " ".join(words)
    return sentence[:1].upper() + sentence[1:]


def load_punctuation():
    directory = os.environ.get("QOL_PUNCT_MODEL", "")
    if not directory:
        return None
    config = sherpa_onnx.OnlinePunctuationConfig(
        model_config=sherpa_onnx.OnlinePunctuationModelConfig(
            cnn_bilstm=first_existing(directory, "model.int8.onnx", "model.onnx"),
            bpe_vocab=os.path.join(directory, "bpe.vocab"),
        )
    )
    return sherpa_onnx.OnlinePunctuation(config).add_punctuation_with_case


def timed_words(result) -> list[tuple[str, float]]:
    words: list[tuple[str, float]] = []
    for token, stamp in zip(result.tokens, result.timestamps):
        if token.startswith(" ") or not words:
            words.append((token.strip(), result.start_time + stamp))
        else:
            words[-1] = (words[-1][0] + token, words[-1][1])
    return [(word, stamp) for word, stamp in words if word]


class Segmenter:
    def __init__(self, frames, punctuate) -> None:
        self.frames = frames
        self.punctuate = punctuate
        self.commit_words = int(os.environ.get("QOL_STT_COMMIT_WORDS", "5"))
        self.lookahead = int(os.environ.get("QOL_STT_COMMIT_LOOKAHEAD", "2"))
        self.committed = 0
        self.start_ns = 0

    def format(self, words: list[str]) -> str:
        text = " ".join(words).lower()
        return self.punctuate(text) if self.punctuate else normalize(text)

    def emit(self, words: list[str], end_ns: int) -> None:
        text = self.format(words).strip()
        if text:
            write_utterance(self.frames, self.start_ns, max(end_ns, self.start_ns), text)
        self.start_ns = max(end_ns, self.start_ns)

    def partial(self, result) -> None:
        if self.punctuate is None or self.commit_words <= 0:
            return
        words = timed_words(result)
        pending = words[self.committed :]
        if len(pending) < 3 + self.lookahead:
            return
        marked = self.punctuate(" ".join(word for word, _ in pending).lower()).split()
        if len(marked) != len(pending):
            return
        boundary = -1
        for index in range(len(pending) - self.lookahead - 1, -1, -1):
            mark = marked[index][-1]
            if (mark in ".?!" and index + 1 >= 3) or (mark in ",;:" and index + 1 >= self.commit_words):
                boundary = index
                break
        if boundary < 0:
            return
        end_ns = seconds_ns(pending[boundary + 1][1])
        self.emit([word for word, _ in pending[: boundary + 1]], end_ns)
        self.committed += boundary + 1

    def endpoint(self, result, end_ns: int) -> None:
        words = [word for word, _ in timed_words(result)][self.committed :]
        self.emit(words, end_ns)
        self.committed = 0


def main() -> None:
    frames = claim_stdout()
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
        rule2_min_trailing_silence=float(os.environ.get("QOL_STT_TRAILING_SILENCE", "0.6")),
        rule3_min_utterance_length=float(os.environ.get("QOL_STT_MAX_UTTERANCE", "4.0")),
    )
    segmenter = Segmenter(frames, load_punctuation())
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
    received = 0
    try:
        while True:
            data = ffmpeg.stdout.read(FRAME_BYTES)
            if not data:
                break
            samples = np.frombuffer(data, dtype=np.int16).astype(np.float32) / 32768.0
            received += len(samples)
            stream.accept_waveform(SAMPLE_RATE, samples)
            while recognizer.is_ready(stream):
                recognizer.decode_stream(stream)
            if recognizer.is_endpoint(stream):
                segmenter.endpoint(recognizer.get_result_all(stream), sample_ns(received))
                recognizer.reset(stream)
            else:
                segmenter.partial(recognizer.get_result_all(stream))
        tail = np.zeros(int(0.5 * SAMPLE_RATE), dtype=np.float32)
        stream.accept_waveform(SAMPLE_RATE, tail)
        stream.input_finished()
        while recognizer.is_ready(stream):
            recognizer.decode_stream(stream)
        segmenter.endpoint(recognizer.get_result_all(stream), sample_ns(received))
    finally:
        if ffmpeg.poll() is None:
            ffmpeg.terminate()
        ffmpeg.wait()


if __name__ == "__main__":
    main()

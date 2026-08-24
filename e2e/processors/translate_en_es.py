from __future__ import annotations

import os
import sys

import ctranslate2
import sentencepiece as spm
from huggingface_hub import snapshot_download


def translate_text(text: str) -> str:
    model_id = "Helsinki-NLP/opus-mt-en-es"
    model_dir = os.environ.get(
        "QOL_TRANSLATE_MODEL",
        os.path.expanduser("~/.cache/sermon_translate/opus-mt-en-es-ct2"),
    )
    huggingface_dir = snapshot_download(model_id, local_files_only=True)
    source = spm.SentencePieceProcessor()
    source.load(os.path.join(huggingface_dir, "source.spm"))
    target = spm.SentencePieceProcessor()
    target.load(os.path.join(huggingface_dir, "target.spm"))
    translator = ctranslate2.Translator(model_dir, device="cpu", compute_type="int8")
    tokens = source.encode(text, out_type=str)
    pieces: list[str] = []
    for start in range(0, len(tokens), 450):
        window = tokens[start : start + 450] + ["</s>"]
        result = translator.translate_batch([window], beam_size=1)[0].hypotheses[0]
        piece = target.decode(result).strip()
        if piece:
            pieces.append(piece)
    spanish = " ".join(pieces)
    if not spanish:
        raise RuntimeError("Translation produced no Spanish text")
    return spanish


def main() -> None:
    text = sys.stdin.read().strip()
    if not text:
        return
    sys.stdout.write(translate_text(text))


if __name__ == "__main__":
    main()

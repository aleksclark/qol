from __future__ import annotations

import os
import sys

import ctranslate2
import sentencepiece as spm
from huggingface_hub import snapshot_download


class Translator:
    def __init__(self) -> None:
        model_id = os.environ.get("QOL_TRANSLATE_HF_MODEL", "Helsinki-NLP/opus-mt-tc-big-en-es")
        model_dir = os.environ.get(
            "QOL_TRANSLATE_MODEL",
            os.path.expanduser("~/.cache/qol/models/opus-mt-tc-big-en-es-ct2"),
        )
        self.beam_size = int(os.environ.get("QOL_TRANSLATE_BEAM", "4"))
        huggingface_dir = snapshot_download(model_id, local_files_only=True)
        self.source = spm.SentencePieceProcessor()
        self.source.load(os.path.join(huggingface_dir, "source.spm"))
        self.target = spm.SentencePieceProcessor()
        self.target.load(os.path.join(huggingface_dir, "target.spm"))
        self.translator = ctranslate2.Translator(model_dir, device="cpu", compute_type="int8")

    def translate(self, text: str) -> str:
        tokens = self.source.encode(text, out_type=str)
        pieces: list[str] = []
        for start in range(0, len(tokens), 450):
            window = tokens[start : start + 450] + ["</s>"]
            result = self.translator.translate_batch([window], beam_size=self.beam_size)[0].hypotheses[0]
            piece = self.target.decode(result).strip()
            if piece:
                pieces.append(piece)
        spanish = " ".join(pieces).replace("\n", " ")
        if not spanish:
            raise RuntimeError("Translation produced no Spanish text")
        return spanish


def main() -> None:
    translator = Translator()
    for line in sys.stdin:
        text = line.strip()
        if not text:
            sys.stdout.write("\n")
        else:
            sys.stdout.write(translator.translate(text) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    main()

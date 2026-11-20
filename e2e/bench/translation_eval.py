from __future__ import annotations

import argparse
import json
import os
import re
import statistics
import subprocess
import time
from collections.abc import Callable

import sacrebleu

PUNCTUATION = re.compile(r"[^\w\s']", re.UNICODE)


def asr_style(text: str) -> str:
    words = PUNCTUATION.sub(" ", text).lower().split()
    words = ["I" if word == "i" else "I" + word[1:] if word.startswith("i'") else word for word in words]
    sentence = " ".join(words)
    return sentence[:1].upper() + sentence[1:]


def load_test_set(name: str, pair: str, limit: int) -> tuple[list[str], list[str]]:
    def echo(side: str) -> list[str]:
        output = subprocess.run(["sacrebleu", "-t", name, "-l", pair, "--echo", side], check=True, capture_output=True, text=True).stdout
        return output.splitlines()[:limit]

    return echo("src"), echo("ref")


def punctuator() -> Callable[[str], str]:
    import sherpa_onnx

    directory = os.environ["QOL_PUNCT_MODEL"]
    config = sherpa_onnx.OnlinePunctuationConfig(
        model_config=sherpa_onnx.OnlinePunctuationModelConfig(
            cnn_bilstm=os.path.join(directory, "model.int8.onnx"),
            bpe_vocab=os.path.join(directory, "bpe.vocab"),
        )
    )
    model = sherpa_onnx.OnlinePunctuation(config)
    return model.add_punctuation_with_case


def marian_ct2(model_id: str, ct2_dir: str, device: str) -> Callable[[str], str]:
    import ctranslate2
    import sentencepiece
    from huggingface_hub import snapshot_download

    snapshot = snapshot_download(model_id)
    if not os.path.isdir(ct2_dir):
        subprocess.run(["ct2-transformers-converter", "--model", snapshot, "--output_dir", ct2_dir, "--quantization", "int8"], check=True)
    source = sentencepiece.SentencePieceProcessor(model_file=os.path.join(snapshot, "source.spm"))
    target = sentencepiece.SentencePieceProcessor(model_file=os.path.join(snapshot, "target.spm"))
    translator = ctranslate2.Translator(ct2_dir, device=device, compute_type="int8")

    def translate(text: str) -> str:
        tokens = source.encode(text, out_type=str) + ["</s>"]
        result = translator.translate_batch([tokens], beam_size=4)[0].hypotheses[0]
        return target.decode(result)

    return translate


def gpu_dtype():
    import torch

    name = os.environ.get("QOL_EVAL_DTYPE")
    if name:
        return getattr(torch, name)
    return torch.bfloat16 if torch.cuda.get_device_capability()[0] >= 8 else torch.float16


def madlad(model_id: str) -> Callable[[str], str]:
    from transformers import AutoTokenizer, T5ForConditionalGeneration

    tokenizer = AutoTokenizer.from_pretrained(model_id)
    model = T5ForConditionalGeneration.from_pretrained(model_id, dtype=gpu_dtype(), device_map="cuda:0")

    def translate(text: str) -> str:
        inputs = tokenizer("<2es> " + text, return_tensors="pt").to("cuda:0")
        output = model.generate(**inputs, max_new_tokens=256, num_beams=4)
        return tokenizer.decode(output[0], skip_special_tokens=True)

    return translate


def instruct_llm(model_id: str) -> Callable[[str], str]:
    from transformers import AutoModelForCausalLM, AutoTokenizer

    tokenizer = AutoTokenizer.from_pretrained(model_id)
    model = AutoModelForCausalLM.from_pretrained(model_id, dtype=gpu_dtype(), device_map="cuda:0")
    system = (
        "You are a live interpreter translating a church sermon from English to Spanish. "
        "The English comes from speech recognition and may lack punctuation. "
        "Reply with only the Spanish translation."
    )

    def translate(text: str) -> str:
        messages = [{"role": "system", "content": system}, {"role": "user", "content": text}]
        prompt = tokenizer.apply_chat_template(messages, tokenize=False, add_generation_prompt=True, enable_thinking=False)
        inputs = tokenizer(prompt, return_tensors="pt").to("cuda:0")
        output = model.generate(**inputs, max_new_tokens=256, do_sample=False)
        return tokenizer.decode(output[0][inputs["input_ids"].shape[1] :], skip_special_tokens=True).strip()

    return translate


def release_gpu() -> None:
    import gc

    gc.collect()
    try:
        import torch

        torch.cuda.empty_cache()
    except ImportError:
        pass


def build(name: str) -> Callable[[str], str]:
    cache = os.path.expanduser("~/.cache/qol/models")
    if name == "opus-mt-en-es":
        return marian_ct2("Helsinki-NLP/opus-mt-en-es", os.path.join(cache, "opus-mt-en-es-ct2"), "cpu")
    if name == "opus-mt-tc-big-en-es":
        return marian_ct2("Helsinki-NLP/opus-mt-tc-big-en-es", os.path.join(cache, "opus-mt-tc-big-en-es-ct2"), "cpu")
    if name == "madlad400-3b":
        return madlad("google/madlad400-3b-mt")
    if name.startswith("Qwen/"):
        return instruct_llm(name)
    raise ValueError(name)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--models", default="opus-mt-en-es,opus-mt-tc-big-en-es")
    parser.add_argument("--limit", type=int, default=300)
    parser.add_argument("--test-set", default="wmt13")
    args = parser.parse_args()
    sources, references = load_test_set(args.test_set, "en-es", args.limit)
    restore = punctuator()
    conditions = {
        "clean": sources,
        "asr": [asr_style(text) for text in sources],
    }
    conditions["asr+punct"] = [restore(text) for text in conditions["asr"]]
    for name in args.models.split(","):
        translate = build(name)
        translate("Warm up.")
        for condition, inputs in conditions.items():
            hypotheses = []
            latencies = []
            for text in inputs:
                started = time.monotonic()
                hypotheses.append(translate(text))
                latencies.append(time.monotonic() - started)
            chrf = sacrebleu.corpus_chrf(hypotheses, [references], word_order=2).score
            bleu = sacrebleu.corpus_bleu(hypotheses, [references]).score
            print(
                json.dumps(
                    {
                        "model": name,
                        "condition": condition,
                        "sentences": len(inputs),
                        "chrf++": round(chrf, 2),
                        "bleu": round(bleu, 2),
                        "median_ms": round(statistics.median(latencies) * 1000),
                        "p90_ms": round(sorted(latencies)[int(len(latencies) * 0.9)] * 1000),
                    }
                ),
                flush=True,
            )
        del translate
        release_gpu()


if __name__ == "__main__":
    main()

from __future__ import annotations

import argparse
import json
import re
import time

import torch
from transformers import AutoModelForCausalLM, AutoTokenizer

SYSTEM = (
    "You are a live English-to-Spanish interpreter for a church sermon. English arrives in fragments "
    "from speech recognition; fragments may stop mid-sentence. For each fragment, reply with only the "
    "Spanish that continues your previous output so the concatenation reads as fluent Spanish. Do not "
    "repeat earlier Spanish. If a fragment cannot be rendered yet, reply with an empty line. Keep "
    "names and terms consistent with earlier turns. When a fragment is marked [CONDENSE], say the same "
    "meaning in as few Spanish words as possible."
)


class IncrementalTranslator:
    def __init__(self, model_id: str, window: int) -> None:
        self.tokenizer = AutoTokenizer.from_pretrained(model_id)
        dtype = torch.bfloat16 if torch.cuda.get_device_capability()[0] >= 8 else torch.float16
        self.model = AutoModelForCausalLM.from_pretrained(model_id, dtype=dtype, device_map="cuda:0")
        self.window = window
        self.messages: list[dict[str, str]] = []
        self.cache = None
        self.cached: list[int] = []

    def _prompt_ids(self) -> list[int]:
        messages = [{"role": "system", "content": SYSTEM}] + self.messages[-2 * self.window - 1 :]
        text = self.tokenizer.apply_chat_template(messages, tokenize=False, add_generation_prompt=True)
        return self.tokenizer(text, add_special_tokens=False).input_ids

    def step(self, fragment: str, condense: bool, reuse: bool) -> tuple[str, float, int]:
        self.messages.append({"role": "user", "content": ("[CONDENSE] " if condense else "") + fragment})
        ids = self._prompt_ids()
        if not reuse:
            self.cache, self.cached = None, []
        common = 0
        for left, right in zip(self.cached, ids):
            if left != right:
                break
            common += 1
        if self.cache is not None and common < len(self.cached):
            self.cache.crop(common)
            self.cached = self.cached[:common]
        if self.cache is not None and common == len(ids):
            self.cache.crop(common - 1)
            self.cached = self.cached[: common - 1]
        started = time.monotonic()
        inputs = torch.tensor([ids], device="cuda:0")
        output = self.model.generate(
            input_ids=inputs,
            past_key_values=self.cache,
            max_new_tokens=96,
            do_sample=False,
            return_dict_in_generate=True,
        )
        torch.cuda.synchronize()
        elapsed = time.monotonic() - started
        sequence = output.sequences[0].tolist()
        self.cache = output.past_key_values
        self.cached = sequence[:-1]
        spanish = self.tokenizer.decode(sequence[len(ids) :], skip_special_tokens=True).strip()
        self.messages.append({"role": "assistant", "content": spanish})
        return spanish, elapsed, len(ids) - common


def syllables(text: str) -> int:
    return sum(max(1, len(re.findall(r"[aeiouáéíóúü]+", word))) for word in re.findall(r"[a-záéíóúüñ]+", text.lower()))


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", default="Qwen/Qwen3-4B-Instruct-2507")
    parser.add_argument("--utterances", required=True)
    parser.add_argument("--window", type=int, default=12)
    args = parser.parse_args()
    with open(args.utterances) as source:
        fragments = [item["text"] for item in json.load(source)]
    translator = IncrementalTranslator(args.model, args.window)
    translator.step("Good morning.", False, True)
    for label, condense, reuse in (("normal+reuse", False, True), ("normal+no-reuse", False, False), ("condense+reuse", True, True)):
        translator.messages, translator.cache, translator.cached = [], None, []
        latencies, prefill, output = [], [], []
        for fragment in fragments:
            spanish, elapsed, new_tokens = translator.step(fragment, condense, reuse)
            latencies.append(elapsed)
            prefill.append(new_tokens)
            output.append(spanish)
        latencies.sort()
        print(json.dumps({
            "mode": label,
            "fragments": len(fragments),
            "median_ms": round(latencies[len(latencies) // 2] * 1000),
            "p90_ms": round(latencies[int(len(latencies) * 0.9)] * 1000),
            "median_prefill_tokens": sorted(prefill)[len(prefill) // 2],
            "spanish_syllables": syllables(" ".join(output)),
        }), flush=True)
        print("  " + " | ".join(output), flush=True)


if __name__ == "__main__":
    main()

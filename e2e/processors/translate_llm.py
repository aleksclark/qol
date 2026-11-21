from __future__ import annotations

import os
import re
import sys
import time

from qol_processor import claim_stdout, processor_pb2, read_frame, write_frame

SYSTEM = (
    "You are a live English-to-Spanish interpreter. English arrives in fragments from speech "
    "recognition; fragments may stop mid-sentence. For each fragment, reply with only the Spanish that "
    "continues your previous output so the concatenation reads as fluent Spanish. Translate only what the "
    "fragment says: never add ideas, never finish a sentence the speaker has not finished, and if the "
    "fragment ends mid-sentence, end your Spanish mid-sentence too. Do not repeat earlier Spanish. If a "
    "fragment cannot be rendered yet, reply with an empty line. Keep names and terms consistent with "
    "earlier turns and the glossary."
)
FINAL = (
    "[END] The speaker has stopped. If some English above has not been rendered in Spanish yet, reply "
    "with only that missing Spanish. Otherwise reply with an empty line."
)
CONDENSE = (
    "You are a live interpreter who has fallen behind the speaker. Translate only the English excerpt into "
    "Spanish, using at most {percent}% of the words a full translation would need. Keep the main point; "
    "drop asides, repetitions and fillers. The preceding Spanish is given only so your text continues it "
    "smoothly: never include or repeat it. Reply with Spanish only."
)
SUMMARIZE = (
    "Summarize, in at most three short Spanish sentences, what the speaker has said so far, for an "
    "interpreter who needs the context. Include names and key terms. Reply with the summary only."
)


class Session:
    def __init__(self) -> None:
        self.turns: list[dict] = []
        self.summary = ""
        self.glossary: dict[str, str] = {}
        self.cache = None
        self.cached: list[int] = []

    def system(self) -> str:
        parts = [SYSTEM]
        if self.glossary:
            parts.append("Glossary: " + "; ".join(f"{source} = {target}" for source, target in self.glossary.items()))
        if self.summary:
            parts.append("Context so far: " + self.summary)
        return "\n".join(parts)


class Translator:
    def __init__(self) -> None:
        import torch
        from transformers import AutoModelForCausalLM, AutoTokenizer

        self.torch = torch
        model_id = os.environ.get("QOL_TRANSLATE_LLM_MODEL", "Qwen/Qwen3-4B-Instruct-2507")
        device = os.environ.get("QOL_TRANSLATE_LLM_DEVICE", "cuda:0")
        if device.startswith("cuda"):
            dtype = torch.bfloat16 if torch.cuda.get_device_capability()[0] >= 8 else torch.float16
        else:
            dtype = torch.float32
        self.device = device
        self.tokenizer = AutoTokenizer.from_pretrained(model_id)
        self.model = AutoModelForCausalLM.from_pretrained(model_id, dtype=dtype, device_map=device)
        self.window = int(os.environ.get("QOL_TRANSLATE_LLM_WINDOW", "20"))
        self.keep = int(os.environ.get("QOL_TRANSLATE_LLM_KEEP", "6"))
        self.sessions: dict[str, Session] = {}

    def generate(self, ids: list[int], session: Session | None, max_new_tokens: int) -> str:
        cache = None
        if session is not None and session.cache is not None:
            common = 0
            for left, right in zip(session.cached, ids):
                if left != right:
                    break
                common += 1
            common = min(common, len(ids) - 1)
            if common > 0:
                session.cache.crop(common)
                cache = session.cache
        inputs = self.torch.tensor([ids], device=self.device)
        output = self.model.generate(
            input_ids=inputs,
            attention_mask=self.torch.ones_like(inputs),
            past_key_values=cache,
            max_new_tokens=max_new_tokens,
            do_sample=False,
            return_dict_in_generate=True,
            pad_token_id=self.tokenizer.eos_token_id,
        )
        sequence = output.sequences[0].tolist()
        if session is not None:
            session.cache = output.past_key_values
            session.cached = sequence[:-1]
        return self.tokenizer.decode(sequence[len(ids) :], skip_special_tokens=True).strip()

    def chat_ids(self, messages: list[dict]) -> list[int]:
        text = self.tokenizer.apply_chat_template(messages, tokenize=False, add_generation_prompt=True)
        return self.tokenizer(text, add_special_tokens=False).input_ids

    def conversation(self, session: Session) -> list[dict]:
        messages = [{"role": "system", "content": session.system()}]
        for turn in session.turns[-self.window :]:
            messages.append({"role": "user", "content": turn["source"]})
            messages.append({"role": "assistant", "content": turn["target"]})
        return messages

    def turn(self, session: Session, turn_id: int, text: str, final: bool) -> str:
        source = text if not final else (FINAL + (" " + text if text else ""))
        messages = self.conversation(session) + [{"role": "user", "content": source}]
        spanish = clean(self.generate(self.chat_ids(messages), session, 96))
        previous = next((turn["target"] for turn in reversed(session.turns) if turn["target"]), "")
        if final and previous and spanish.rstrip(".,;") in previous:
            spanish = ""
        session.turns.append({"id": turn_id, "source": text or "[END]", "target": spanish})
        return spanish

    def condense(self, session: Session, turn_ids: list[int], ratio: float) -> str:
        wanted = set(turn_ids)
        selected = [turn for turn in session.turns if turn["id"] in wanted]
        if not selected:
            return ""
        first = session.turns.index(selected[0])
        before = " ".join(turn["target"] for turn in session.turns[max(0, first - 3) : first] if turn["target"])
        english = " ".join(turn["source"] for turn in selected)
        prompt = CONDENSE.format(percent=round(ratio * 100))
        user = f"Preceding Spanish (do not repeat): «{before[-300:]}»\nEnglish excerpt: «{english}»"
        spanish = clean(self.generate(self.chat_ids([{"role": "system", "content": prompt}, {"role": "user", "content": user}]), None, 128)).strip("«»")
        selected[0]["target"] = spanish
        for turn in selected[1:]:
            turn["target"] = ""
        return spanish

    def summarize(self, session: Session) -> None:
        keep = self.keep
        if len(session.turns) <= keep:
            return
        older, session.turns = session.turns[:-keep], session.turns[-keep:]
        transcript = " ".join(turn["source"] for turn in older)
        previous = f"Earlier summary: {session.summary}\n" if session.summary else ""
        session.summary = clean(self.generate(self.chat_ids([{"role": "system", "content": SUMMARIZE}, {"role": "user", "content": previous + transcript}]), None, 160))
        session.cache, session.cached = None, []

    def handle(self, request) -> tuple[str, int]:
        session = self.sessions.setdefault(request.session_id, Session())
        kind = request.WhichOneof("request")
        text = ""
        if kind == "turn":
            text = self.turn(session, request.turn.turn_id, request.turn.text.strip(), request.turn.final)
        elif kind == "condense":
            text = self.condense(session, list(request.condense.turn_ids), request.condense.target_ratio)
        elif kind == "summarize":
            self.summarize(session)
        elif kind == "glossary":
            for entry in request.glossary.entries:
                session.glossary[entry.source] = entry.target
            session.cache, session.cached = None, []
        elif kind == "end":
            self.sessions.pop(request.session_id, None)
            return "", 0
        return text, len(session.turns)


def clean(text: str) -> str:
    text = re.sub(r"\s+", " ", text).strip()
    return "" if text.lower() in {"nothing", "nada", "(nada)", "[nada]", '""'} else text.strip('"')


def main() -> None:
    output = claim_stdout()
    translator = Translator()
    print("translate-llm ready", file=sys.stderr, flush=True)
    while True:
        request = read_frame(sys.stdin.buffer, processor_pb2.TranslatorRequest())
        if request is None:
            return
        started = time.monotonic()
        text, turns = translator.handle(request)
        write_frame(output, processor_pb2.TranslatorResponse(text=text, context_turns=turns))
        output.flush()
        print(
            f"translate-llm {request.WhichOneof('request')} -> {text!r} in {time.monotonic() - started:.3f}s",
            file=sys.stderr,
            flush=True,
        )


if __name__ == "__main__":
    main()

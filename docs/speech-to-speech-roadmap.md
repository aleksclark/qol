# Speech-to-speech roadmap

Goal: live, low-latency English-to-Spanish speech translation that carries the speaker's speed and emotion through, using either a direct model we can refine or a cascade whose models accept prosody as input.

## Where the system stands

| Area | Before | Now |
| --- | --- | --- |
| Source delivery | Not measured; `ProsodyPayload` existed but was never produced | `prosody` stage measures every utterance (pitch, level, syllable rate, pauses, pitch range, emphasis, acoustic arousal) and attaches it to the text |
| Media alignment | Text events carried the cumulative stream span | STT emits per-utterance source spans; translate and TTS keep them |
| Translate / TTS processes | One process per utterance; translate reloaded its model every time | One long-lived process per worker with protobuf/line framing |
| TTS | espeak-ng, fixed voice | Kokoro-82M (CPU in the dev stack), speed and loudness follow the speaker; espeak and Qwen3-TTS processors use the same protocol |
| Tests | Startup-only Compose e2e; opt-in real graph with no quality check | Unit tests for analysis and stages; opt-in real graph reports source-to-output carry-through correlation |

## Hardware

- Local: RTX 3060 12 GB (Ampere, bf16). Local Docker is rootless with no NVIDIA runtime, so GPU processors run as host processes.
- Nomad `node-6`: two Tesla V100-SXM2-16GB (compute 7.0: fp16 only, no FlashAttention 2) plus a Quadro M2000. After the node's reboot on 2026-11-18 the NVIDIA device plugin and Docker runtime expose both V100s (`device "nvidia/gpu"` with `${device.model} = "Tesla V100-SXM2-16GB"`). Both were allocated to `audiobook-gpu2` dispatches during this work, so no V100 benchmark was run. `deploy/nomad/qwen3-tts-bench.nomad.hcl` is ready.
- Other Nomad nodes are CPU-only.

## Model survey

Verified means we ran it here or read its source; the rest comes from model cards and papers.

### Direct speech-to-speech

| Model | en→es | Expressivity | License | Fit | Verdict |
| --- | --- | --- | --- | --- | --- |
| SeamlessStreaming (Meta) | Yes | Weak; neutral unit vocoder | CC-BY-NC 4.0 | ~2.5B, fits 3060/V100 | Latency baseline only; ~2 s lag, no speed or emotion transfer |
| SeamlessExpressive (Meta) | Yes | Strong: rate, pauses, style | Gated research license | Offline, ~10 GB | Quality reference, not live |
| Hibiki-Zero (Kyutai, 2026) | No (fr/es/pt/de→en) | Voice transfer and prosody | CC-BY-NC-SA 4.0 | 3B, ~8 GB | Best direct architecture; en→es needs fine-tuning (paper reports adaptation with <1000 h); research-only license |
| Hibiki (2025) | No (fr→en) | Voice transfer | CC-BY 4.0 | 2.7B / 1.7B | Commercial-friendly base for a fine-tune, but en→es data is the cost |
| StreamSpeech | No (x→en) | Limited | MIT | Small | Not applicable |
| Qwen3-Omni 30B-A3B | Yes | Talker voice, not source prosody | Apache 2.0 | ~64 GB fp16 | Does not fit one V100 or the 3060 |

No open direct model does live en→es with prosody transfer today. A direct model remains a research track: fine-tune Hibiki or Hibiki-Zero for en→es, keeping the cascade as the source of synthetic aligned data and as the production path.

### Cascade components

| Stage | Candidate | Prosody input/output | Notes |
| --- | --- | --- | --- |
| ASR | sherpa-onnx streaming Zipformer (current) | Utterance spans | CPU, 0.6–1.2 s endpointing; uppercase without punctuation (now case-normalized before translation) |
| ASR | Kyutai STT 1B (en/fr), Parakeet TDT 0.6B v3, Canary-1B-v2 | Word timestamps | GPU upgrades; Canary also does en→es speech translation directly |
| Emotion | emotion2vec+ (MIT) | Categorical emotion | Fills `ProsodyPayload.emotion`; fits CPU or either GPU |
| Emotion | audEERING wav2vec2 A/V/D | Arousal/valence/dominance | Research-only license |
| Translation | opus-mt-en-es CTranslate2 (current) | None | Fast but weak (see transcripts below) |
| Translation | NLLB/MADLAD, small instruct LLM on a V100 | Can preserve emphasis markup | Next quality step |
| TTS | Kokoro-82M (verified) | Numeric `speed`; gain applied after synthesis | Apache 2.0; Spanish voices; RTF 0.016 on 3060, 0.1–0.2 on CPU |
| TTS | Qwen3-TTS 12Hz CustomVoice 0.6B/1.7B (verified) | Natural-language `instruct` | Apache 2.0; Spanish; open package has no streaming generation; see measurements |
| TTS | CosyVoice 3 0.5B | Instructions plus bidirectional streaming | Spanish listed; next expressive candidate to evaluate |
| TTS | Chatterbox Multilingual 0.5B | `exaggeration`, `cfg_weight` (pace), voice clone | MIT; Spanish; numeric emotion intensity |
| TTS | IndexTTS 2 | Explicit duration and 8-way emotion vector | Bilibili license; Spanish support unverified |
| TTS | Fish S2/OpenAudio, Higgs Audio, VibeVoice | Inline emotion tags | Restrictive licenses or unverified Spanish |

## Measurements

90 s of the sermon sample through the real graph (`e2e/graph_test.go`), 23 utterances, all annotated. Correlation is Pearson between the source utterance's relative value and the synthesized Spanish measurement.

| TTS processor | Hardware | TTS speed | Rate r | Level r | Pitch r |
| --- | --- | --- | --- | --- | --- |
| espeak-ng with rate/pitch/amplitude flags | CPU | Near-instant | 0.86 | 0.89 | 0.98 |
| Kokoro-82M with speed and gain | RTX 3060 | RTF 0.016 | 0.72 | 0.96 | 0.33 |
| Kokoro-82M with speed and gain | CPU (also dev Docker) | RTF 0.1–0.25 | 0.72 | 0.96 | 0.33 |
| Qwen3-TTS 1.7B CustomVoice, prosody rendered as `instruct` | RTX 3060 bf16, SDPA | RTF 1.11 (132 s audio in 147 s) | 0.25 | 0.12 | 0.56 |

Standalone Qwen3-TTS runs on the 3060 (same sentence, three samples each): 1.7B averaged 8.4 s of audio for "very fast, excited" and 11.4 s for "very slow, calm", so instructions move delivery in the right direction with high variance. 0.6B showed no rate response. Without streaming, a 6 s utterance costs ~5 s before the first sample, which rules it out for live use on this GPU.

Live dev stack (NATS, Docker, Kokoro on CPU): a 60 s clip streamed at playback rate produced both Ogg/Opus files 0.4 s after the source ended.

Conclusions:

1. Numeric controls carry delivery; instruction prompts do not yet do so reliably or fast enough.
2. Speed and loudness transfer now work at live latency. Pitch contour and emotion are the gap.
3. The translation model is the largest quality problem in the transcripts.

## Next steps

1. **Emotion classification.** Add an emotion2vec+ processor behind the prosody stage to fill `emotion`/`emotion_confidence` per utterance. Gate on the carry-through test plus a labelled clip set.
2. **Pitch and emphasis on the fast path.** Apply relative pitch and range to Kokoro output (WORLD/pyworld F0 rescaling per utterance), then evaluate numeric-control expressive models: Chatterbox `exaggeration` from arousal, CosyVoice 3 streaming with instructions, IndexTTS 2 duration and emotion vectors if Spanish holds up. Keep the processor protocol; add a model only if it beats Kokoro on the carry-through test without breaking RTF < 0.3.
3. **Playout scheduling.** Schedule TTS output against source spans (`ProsodyPayload.pause_before_ms`, span start) so pauses transfer and lag stays bounded; let speed absorb backlog. Record ear-to-ear lag per utterance as a metric.
4. **Latency.** Emit partial STT hypotheses and tune endpointing (currently 0.6 s trailing silence); evaluate Kyutai STT 1B or Parakeet on the 3060. Stream TTS segments (Kokoro already yields per segment).
5. **Translation quality.** Add punctuation and casing before translation (sherpa-onnx online punctuation), then compare NLLB/MADLAD and a small instruct LLM on a V100 that can keep emphasis markers in the target text.
6. **Fleet GPU serving.** When the V100s are free, run `nomad job run deploy/nomad/qwen3-tts-bench.nomad.hcl` from the repository root to measure fp16 Volta throughput. Moving a TTS or translation worker onto `node-6` also needs NATS reachable from the fleet, which the dev stack does not expose.
7. **Direct-model track.** Use SeamlessStreaming as a latency baseline. Start a Hibiki fine-tune study for en→es, generating aligned training pairs with this cascade, and keep it separate from production until it beats the cascade on the same carry-through and latency gates.

## Running the carry-through test

Host processors use separate virtual environments because Qwen3-TTS and Kokoro pin different libraries:

```sh
uv venv ~/.cache/qol/proc-venv
VIRTUAL_ENV=~/.cache/qol/proc-venv uv pip install sherpa-onnx numpy ctranslate2==4.8.1 sentencepiece==0.2.1 huggingface-hub==0.36.0 'protobuf>=7.36.1,<8'
uv venv ~/.cache/qol/kokoro-venv
VIRTUAL_ENV=~/.cache/qol/kokoro-venv uv pip install kokoro==0.9.4 'misaki[en]==0.9.4' scipy 'protobuf>=7.36.1,<8'

QOL_RUN_REAL_GRAPH_E2E=1 \
QOL_SHERPA_MODEL=$HOME/.cache/qol/models/sherpa-onnx-streaming-zipformer-en-2023-06-26 \
QOL_TRANSLATE_MODEL=$HOME/.cache/sermon_translate/opus-mt-en-es-ct2 \
QOL_E2E_SAMPLE=/path/to/english.mp3 QOL_E2E_SECONDS=90 \
QOL_E2E_TTS_PYTHON=$HOME/.cache/qol/kokoro-venv/bin/python QOL_E2E_TTS_PROCESSOR=tts_kokoro.py \
QOL_E2E_MIN_RATE_CORRELATION=0.6 \
go test -count=1 -v -timeout 30m ./e2e/
```

Use `QOL_E2E_TTS_PROCESSOR=tts_spanish.py` for espeak-ng, or `tts_qwen.py` with a Qwen venv (`qwen-tts`, `protobuf>=7.36.1,<8`) and `QOL_QWEN_TTS_SPEAKER`. The test logs per-utterance source and output measurements, the correlations, and the Spanish Ogg/Opus path.

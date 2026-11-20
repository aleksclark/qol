# Speech-to-speech roadmap

Goal: live, low-latency English-to-Spanish speech translation that carries the speaker's speed and emotion through, using either a direct model we can refine or a cascade whose models accept prosody as input.

## Where the system stands

| Area | Originally | Round 1 (PR #4) | Round 2 |
| --- | --- | --- | --- |
| Source delivery | Not measured | Pitch, level, syllable rate, pauses, range, emphasis, acoustic arousal per utterance | Adds relative pitch range and categorical emotion from emotion2vec+ (CPU, ~0.15 s per utterance) |
| Segmentation | 0.6–1.2 s silence or 5 s cap | Same, with utterance spans | Online punctuation and casing; clause-boundary early commits; 4 s cap |
| Translation | opus-mt-en-es, model reloaded per utterance | Long-lived process | opus-mt-tc-big-en-es, beam 4, on punctuated input |
| TTS | espeak-ng, fixed voice | Kokoro speed and gain follow the speaker | Kokoro pitch level and range set inside the model's F0 predictor |
| Timing | Back-to-back output | Back-to-back output | Output scheduled on the source timeline: silence when ahead, faster pace while behind |
| Evaluation | Startup-only | Opt-in real graph with rate/level/pitch correlation | Adds range correlation, onset lag, emotion agreement, synthesis fixtures, `qol-eval`, and Nomad GPU jobs |

## Hardware

- Local: RTX 3060 12 GB (Ampere, bf16). Local Docker is rootless with no NVIDIA runtime, so GPU processors run as host processes. The desktop and other applications commonly hold 3–5 GB, so 4B-parameter models do not fit beside them.
- Nomad `node-6`: two Tesla V100-SXM2-16GB plus a Quadro M2000, exposed through the NVIDIA device plugin (`device "nvidia/gpu"` with `${device.model} = "Tesla V100-SXM2-16GB"`). Volta constraints found in practice:
  - PyTorch cu128+ wheels omit sm_70 (`no kernel image is available`); use cu126 (torch 2.8) or cu124.
  - `torch.cuda.is_bf16_supported()` reports True through emulation; Qwen3-TTS then fails. fp16 trips a device-side assert in its sampler, so it only runs in fp32.
  - Jobs share a `qol-python-cache` Docker volume to avoid re-downloading wheels and models.
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
| ASR | sherpa-onnx streaming Zipformer (current) | Utterance spans, token timestamps | CPU; greedy output is append-only, so clause-boundary early commits are safe |
| Punctuation | sherpa-onnx online CNN-BiLSTM punctuation (current, verified) | Restores punctuation and casing | ~5 ms per utterance on CPU; drives early commits |
| ASR | Kyutai STT 1B (en/fr), Parakeet TDT 0.6B v3, Canary-1B-v2 | Word timestamps | GPU upgrades; Canary also does en→es speech translation directly |
| Emotion | emotion2vec+ base (MIT, current, verified) | Categorical emotion and scores | ~0.1–0.3 s per utterance on CPU; also used as the output judge |
| Emotion | audEERING wav2vec2 A/V/D | Arousal/valence/dominance | Research-only license |
| Translation | opus-mt-tc-big-en-es CTranslate2 int8 (current, verified) | None | Best quality per millisecond in the WMT13 check below |
| Translation | MADLAD-400 3B, Qwen3-4B-Instruct (verified) | LLM could keep emphasis markup | Slower and not better on this test |
| TTS | Kokoro-82M (current, verified) | Numeric `speed`; F0 predictor shift and range; gain after synthesis | Apache 2.0; Spanish voices; RTF 0.016 on 3060, 0.1–0.2 on CPU |
| TTS | Qwen3-TTS 12Hz CustomVoice 0.6B/1.7B (verified) | Natural-language `instruct` | Apache 2.0; Spanish; open package has no streaming generation; see measurements |
| TTS | CosyVoice 3 0.5B | Instructions plus bidirectional streaming | Spanish listed; next expressive candidate to evaluate |
| TTS | Chatterbox Multilingual 0.5B (verified on V100) | `exaggeration`, `cfg_weight` (pace), voice clone | MIT; Spanish; RTF ~1.0 on V100; needs `setuptools<81` for its watermarker |
| TTS | IndexTTS 2 | Explicit duration and 8-way emotion vector | Bilibili license; Spanish support unverified |
| TTS | Fish S2/OpenAudio, Higgs Audio, VibeVoice | Inline emotion tags | Restrictive licenses or unverified Spanish |

## Measurements

### Round 1

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

### Round 2

**Translation** (`e2e/bench/translation_eval.py`, first 300 sentences of WMT13 en→es, chrF++ / BLEU). "ASR" lowercases and strips punctuation like the recognizer; "+punct" restores it with the online punctuation model.

| Model | Hardware | Clean | ASR | ASR + punct | Median latency |
| --- | --- | --- | --- | --- | --- |
| opus-mt-en-es (round 1) | CPU int8 | 59.5 / 36.7 | 54.2 / 25.7 | 55.3 / 28.8 | 49 ms |
| opus-mt-tc-big-en-es (now) | CPU int8 | 59.9 / 37.0 | 55.6 / 28.3 | 56.2 / 29.9 | 164 ms |
| MADLAD-400 3B | RTX 3060 bf16 | 60.9 / 38.6 | 55.8 / 27.8 | 56.7 / 30.7 | 697 ms |
| Qwen3-4B-Instruct-2507 | V100 fp16 | 57.4 / 33.5 | 55.4 / 28.8 | 54.7 / 27.5 | ~1.5 s |

Restoring punctuation is worth about +1 chrF and +1.5–3 BLEU for the dedicated MT models; the LLM copes with unpunctuated input and slightly prefers it. The bigger Marian model adds about +1 chrF at 160 ms on CPU. Neither the 3B MT model nor the 4B LLM justifies its latency.

**Graph** (same 90 s, Kokoro on CPU, final defaults: punctuation commits after 5 words, 4 s cap, 1 s playout delay, pace up to 1.4):

| Configuration | Utterances | Rate r | Level r | Pitch r | Range r | Onset lag median / p90 |
| --- | --- | --- | --- | --- | --- | --- |
| Round 1 (speed and gain only, back-to-back output) | 23 | 0.72 | 0.96 | 0.33 | n/a | n/a |
| + Kokoro F0 control | 23 | 0.73 | 0.96 | 0.91 | n/a | n/a |
| + timeline, 1.5 s delay, 5 s cap, no punctuation | 23 | 0.77 | 0.94 | 0.86 | 0.78 | 6.9 s / 7.9 s |
| + punctuation commits (8 words), 1 s delay, pace 1.4 | 31 | 0.71 | 0.98 | 0.88 | 0.79 | 6.0 s / 6.7 s |
| + commits after 5 words, 4 s cap (defaults) | 39 | 0.75–0.80 | 0.97–0.98 | 0.72–0.77 | 0.75–0.77 | 4.9–5.1 s / 5.5–6.0 s |
| 3 s cap, 0.8 s delay | 37 | 0.80 | 0.97 | 0.76 | 0.90 | 4.3 s / 5.5 s |

Onset lag runs from the start of the English utterance to the start of the Spanish speech for it, so its floor is the utterance length plus the playout delay. Shorter segments lower lag but hand the translator sentence fragments.

**Expressive TTS on the fleet** (same 39-utterance fixture, scored with `qol-eval`):

| TTS | Hardware | RTF | Rate r | Level r | Pitch r | Range r | Non-neutral emotion carried |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Kokoro with speed, F0, gain | CPU | 0.1–0.25 | 0.76 | 0.98 | 0.77 | 0.75 | 1 / 11 |
| Chatterbox Multilingual, `exaggeration` from arousal, `cfg_weight` from rate | V100 fp32 | 1.02 | 0.19 | 0.53 | −0.06 | 0.23 | 0 / 11 |
| Qwen3-TTS 0.6B / 1.7B CustomVoice (benchmark sentence) | V100 fp32 | 2.1 | 1.7B: "slow" instructions ~47% longer than "fast" | | | | |

**Live dev stack** (NATS, Docker, all CPU, emotion on): a 60 s clip streamed at playback rate. The time-aligned Spanish track was 63.3 s long, and both files were written 1.1 s after the source ended. Scheduled lag was 2.9–6.0 s, and no warnings or errors were logged.

Conclusions:

1. Speed, loudness, pitch level, and pitch range now carry through at live latency on CPU.
2. Categorical emotion does not carry with any model tried. The source classifier works, but neither Kokoro's numeric controls nor Chatterbox's `exaggeration` reproduces an angry or happy reading in Spanish. Note that emotion2vec+ labels emphatic preaching as angry or happy fairly often, so its judgments need a human-labelled check.
3. Expressive autoregressive TTS (Qwen3-TTS, Chatterbox) runs at or slower than real time on the available GPUs, so it cannot sit on the live path yet.
4. Lag is now dominated by segment length. Going below ~4 s needs streaming translation, not further tuning.

## Next steps

1. **Emotion by reference audio.** Send the source utterance's audio (or a clip from an emotion-matched bank) to the synthesizer as a style reference. Candidates are Chatterbox's `audio_prompt_path`, IndexTTS 2's separate emotion prompt (check Spanish), and CosyVoice 3. Keep Kokoro as the live fallback. Judge with the emotion-agreement metric plus a human-labelled set.
2. **Faster expressive synthesis.** Expressive models need RTF below 0.3. Try CosyVoice 3 streaming with vLLM, and TensorRT/vLLM serving for Chatterbox on the V100s.
3. **Streaming translation.** Use wait-k or retranslation over the append-only ASR prefix so speech can start before a clause finishes. Evaluate on both lag and chrF.
4. **ASR upgrade.** Compare Kyutai STT 1B and Parakeet TDT against Zipformer for accuracy and word timing. Word timing would enable word-level emphasis transfer.
5. **Fleet serving.** To run workers on `node-6`, NATS must be reachable from the fleet. Either deploy NATS on Nomad or expose the dev NATS port.
6. **Direct-model track.** Use SeamlessStreaming as a lag baseline. Start a Hibiki fine-tune study for en→es, with the cascade generating aligned training pairs.

## Running the evaluations

Host processors use separate virtual environments because the TTS models pin different libraries:

```sh
uv venv ~/.cache/qol/proc-venv
VIRTUAL_ENV=~/.cache/qol/proc-venv uv pip install sherpa-onnx numpy ctranslate2==4.8.1 sentencepiece==0.2.1 huggingface-hub==0.36.0 'protobuf>=7.36.1,<8'
uv venv ~/.cache/qol/kokoro-venv
VIRTUAL_ENV=~/.cache/qol/kokoro-venv uv pip install kokoro==0.9.4 'misaki[en]==0.9.4' scipy 'protobuf>=7.36.1,<8'
uv venv ~/.cache/qol/emo-venv
VIRTUAL_ENV=~/.cache/qol/emo-venv uv pip install --extra-index-url https://download.pytorch.org/whl/cpu 'torch==2.8.0+cpu' 'torchaudio==2.8.0+cpu' funasr huggingface-hub 'protobuf>=7.36.1,<8'
```

Download the Zipformer and punctuation models from the sherpa-onnx releases into `~/.cache/qol/models`. Convert `Helsinki-NLP/opus-mt-tc-big-en-es` with `ct2-transformers-converter --quantization int8` into `~/.cache/qol/models/opus-mt-tc-big-en-es-ct2`.

Real graph, with gates:

```sh
QOL_RUN_REAL_GRAPH_E2E=1 QOL_SHERPA_MODEL=$HOME/.cache/qol/models/sherpa-onnx-streaming-zipformer-en-2023-06-26 QOL_PUNCT_MODEL=$HOME/.cache/qol/models/sherpa-onnx-online-punct-en-2024-08-06 QOL_E2E_SAMPLE=/path/to/english.mp3 QOL_E2E_SECONDS=90 QOL_E2E_TTS_PYTHON=$HOME/.cache/qol/kokoro-venv/bin/python QOL_E2E_TTS_PROCESSOR=tts_kokoro.py QOL_E2E_EMOTION_PYTHON=$HOME/.cache/qol/emo-venv/bin/python QOL_E2E_MIN_RATE_CORRELATION=0.6 QOL_E2E_MIN_PITCH_CORRELATION=0.6 QOL_E2E_MAX_P90_LAG=7s QOL_E2E_DUMP_SYNTHESIS=$PWD/tmp/fixtures/sermon90.bin go test -count=1 -v -timeout 30m ./e2e/
```

`QOL_E2E_PLAYOUT_DELAY`, `QOL_E2E_MAX_PACE`, `QOL_STT_COMMIT_WORDS`, and `QOL_STT_MAX_UTTERANCE` override the latency settings.

A TTS on the V100 fleet, scored locally against the same fixture:

```sh
nomad job run -var processor=tts_chatterbox.py -var 'packages=chatterbox-tts setuptools==80.9.0' \
  -var torch_index=https://download.pytorch.org/whl/cu124 -var fixture=tmp/fixtures/sermon90.bin \
  deploy/nomad/tts-carry-through.nomad.hcl
nomad alloc fs <alloc> tts/local/audio.bin > tmp/fixtures/sermon90-chatterbox.bin
go run ./cmd/qol-eval carry-through --requests tmp/fixtures/sermon90.bin --audio tmp/fixtures/sermon90-chatterbox.bin \
  --emotion-processor $HOME/.cache/qol/emo-venv/bin/python --emotion-processor-arg e2e/processors/emotion_emotion2vec.py
```

Translation quality on the fleet: `nomad job run -var 'models=Qwen/Qwen3-4B-Instruct-2507' deploy/nomad/translation-eval.nomad.hcl`. Locally: `python e2e/bench/translation_eval.py --models opus-mt-en-es,opus-mt-tc-big-en-es`, with `sacrebleu`, `ctranslate2`, `sherpa-onnx`, and `QOL_PUNCT_MODEL` set.

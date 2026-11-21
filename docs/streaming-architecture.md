# Streaming architecture proposal

Status: phases P0, P1, and part of P3 are implemented (see "Implementation status" at the end). Decisions: the translator reads committed words only; Seed-X and Nemotron licenses are acceptable; there is no human reference translation yet.

## Why change

Today every stage works on whole utterances. The recognizer cuts the English into pieces (at clause boundaries, or hard at 4 s). Each piece is translated alone, synthesized alone, and butted against its neighbours. That produces the defects heard in the Spanish track:

- **Mid-word cuts:** for example "Y el resto de AP" followed by "Ostles poner…".
- **Fragments translated without context:** for example "Así que aquí está el" followed by "Cosa".
- **Resets at every boundary:** intonation, speed, pitch, and loudness all start over.
- **Hard joins:** 7 abrupt onsets and 7 click candidates, every one at a piece boundary.

Lag is bounded below by the piece length, so it cannot fall much under the current 5 s median.

The goal is a fully streamed graph:
- every stage consumes and produces continuous streams;
- every stage keeps the context it needs;
- every stage accepts control inputs as well as data, so a coordinator can steer it, for example "condense what you have not said yet; we are 6 s behind".

## Principles

1. **Streams, not utterances.** Data flows as deltas on a session timeline. A delta is either *tentative* (it may be replaced) or *committed* (immutable). Every delta carries the source media span it covers.
2. **Three planes per stage.** Each stage has *data* in and out, *control* in (typed directives), and *telemetry* out (watermarks and backlog). The current `qol.Stage` ports gain a control port and a telemetry port.
3. **Stateful, session-affine stages.** The translator keeps dialogue context and the synthesizer keeps acoustic context, so each session is pinned to one worker instance per stage. Assignment is explicit. It is not the result of a NATS queue group.
4. **Closed loop.** A `conductor` stage reads telemetry and prosody and issues control: speed nudges, condensing, glossary updates, skips. Policies live there, not inside the model workers.
5. **Speak only what is committed.** The synthesizer voices committed target text. Text it has accepted but not yet spoken stays revisable, which is what makes "condense the backlog" possible.

## Graph

```text
mic/WebTransport ─► capture ─► asr ─(words: tentative|committed + times)─► translator ─(es text deltas)─► tts ─(PCM)─► playout ─► listeners / record-es
                      │          │                                          ▲      │                    ▲   │            │
                      │          └─► prosody ─(word/frame features, emotion, reference audio)──────────┼───┼────────────┘
                      │                                                     │      │                    │   │
                      └─► record-en                     conductor ◄── telemetry (watermarks, backlog, lag) from every stage
                                                            │
                                                            └── control ──► asr / translator / tts / playout
```

## Contracts (protobuf sketch)

```proto
message StreamDelta {
  string stream = 1;            // "asr.words", "translation.es", "tts.audio"
  uint64 revision = 2;          // increases on every replace
  uint64 committed_through = 3; // position (word or token index) that is now immutable
  repeated Token tokens = 4;    // replacement tail starting at tokens[0].index
  MediaSpan span = 5;           // source media covered
  ProsodyPayload prosody = 6;   // optional, aligned to the tokens
}

message Token { uint64 index = 1; string text = 2; MediaSpan span = 3; float confidence = 4; }

message Control {
  string stage = 1;
  oneof directive {
    Condense condense = 2;        // scope = unspoken backlog; target_ratio, deadline
    SetPace set_pace = 3;         // tts: continuous speed multiplier, ramp time
    SetStyle set_style = 4;       // tts: emotion/instruction or reference-audio id
    GlossaryUpdate glossary = 5;  // translator: term pairs, names
    Summarize summarize = 6;      // translator: fold old context into a running summary
    Flush flush = 7;              // finish and commit what is pending (end of speech)
    Skip skip = 8;                // drop unspoken material up to a position
    Resync resync = 9;            // re-send from a sequence after a detected gap
  }
}

message Telemetry {
  string stage = 1;
  MediaTime consumed_through = 2;  // source time fully consumed
  MediaTime committed_through = 3; // source time whose output is committed
  double backlog_seconds = 4;      // work accepted but not yet emitted or spoken
  double processing_ms = 5;
  uint64 spoken_token_index = 6;   // tts/playout: last target token actually voiced
}
```

**Transport.** Core NATS still fits, with no JetStream:
- Subjects are per session: `qol.s.<session>.<stage>.data`, `.control`, `.telemetry`.
- Positions are cumulative, so a receiver detects a gap and sends `Resync`. Senders keep a short in-memory replay buffer (a few seconds) to answer it.
- The conductor assigns sessions to stage instances on `qol.assign.<stage>`, which replaces queue groups for stateful stages.

## Stages and model candidates

Licenses and capabilities were checked against model cards on 2026-11-20. "Volta" means the fleet V100s: sm_70, no bf16, no FlashAttention 2, and no vLLM ≥ 0.20.

### ASR: words with tentative tail and committed prefix

| Candidate | Why | Concerns |
| --- | --- | --- |
| sherpa-onnx streaming Zipformer (current) | Greedy transducer output is append-only, so a committed prefix is free; runs on CPU | No punctuation (the online punctuation model covers it); lower accuracy |
| Kyutai STT 1B en/fr (CC-BY-4.0) | True streaming with a 0.5 s delay; native punctuation and word timestamps; semantic end-of-turn signal; append-only | Semantic VAD only in the Rust server; Volta untested |
| NVIDIA Nemotron speech streaming en 0.6B (NVIDIA Open Model License) | Cache-aware, with latency selectable from 80 ms to 1.1 s; punctuation; word timestamps; Volta listed as supported | Partials are revisable; license review needed |

Proposal: keep Zipformer for the first phase and emit word deltas instead of utterances. Then evaluate Kyutai STT 1B on the 3060 and Nemotron on a V100 for accuracy, commit delay, and end-of-turn quality.

### Prosody

Keep the pure-Go DSP, but emit per-word and per-frame features along the word stream instead of per utterance. Emotion (emotion2vec+) runs over sliding 2–3 s windows. A rolling reference-audio window (the last N seconds of speech) is exposed for synthesizer style conditioning.

### Translator: the stateful core

| Candidate | License | Notes |
| --- | --- | --- |
| Qwen3-4B-Instruct-2507 | Apache-2.0 | Spike below: fluent continuation across fragments; follows condense instructions on a backlog |
| Seed-X-PPO-7B | OpenMDW | Translation-specialized and RL-tuned; strong reported en→es; instruction following to be tested |
| TranslateGemma 4B/12B | Gemma terms, gated | Translation-specialized; 2k context limits rolling context |
| Hunyuan-MT-7B | Tencent Hunyuan Community License; excludes EU/UK/South Korea | WMT25 results are strong, but the license is territorial |
| Tower+ 9B | CC-BY-NC-SA | Strong instruction-following MT; non-commercial only |
| opus-mt-tc-big (current) | CC-BY | No context and no instructions; fallback only |

**Method: conversational simultaneous MT.**
- Each committed source span (a clause, or every k committed words under a wait-k or LocalAgreement policy) becomes a user turn.
- The model answers with the Spanish that *continues* its previous output.
- The dialogue grows append-only, so the KV cache is reused (prompt caching in llama.cpp or vLLM).

**Context management:**
- a fixed system prompt with role, register, and glossary;
- a running summary of older material, rewritten on `Summarize`;
- the last N turns verbatim;
- the current turn.

**Controls:**
- `Condense(target_ratio, scope=unspoken)` re-renders the not-yet-spoken target backlog more briefly.
- `GlossaryUpdate` adds names and terms such as scripture books or speaker names.
- `Summarize` folds old turns into the summary.
- `Flush` commits the rest at end of speech.
- `SetRegister` changes formality or style.

**Serving:**
- Local 3060: vLLM or llama.cpp with prompt caching.
- V100: llama.cpp (sm_70 supported) or vLLM 0.18.x, in fp16.

### TTS: streaming text in, continuous audio out

| Candidate | Text-in streaming | Spanish | Controls | License | Notes |
| --- | --- | --- | --- | --- | --- |
| Fun-CosyVoice3 0.5B | Yes (generator input, "bi-streaming") | Yes | Instruct (emotion, speed, volume) and zero-shot reference audio | Apache-2.0 | Best fit: continuity plus controls plus emotion by reference. Claims 150 ms first audio; vLLM/TensorRT-LLM serving. Defaults to bf16, so test fp16 on Volta |
| Kokoro-82M (current) | No | Yes | Speed, F0 shift/range (ours), gain | Apache-2.0 | Fast fallback. Needs crossfades and smoothed controls at segment joins |
| Kyutai Pocket TTS 100M | To verify | Yes (2026 multilingual) | Voice cloning | CC-BY-4.0, gated | CPU; worth a spike |
| Kyutai TTS 1.6B | Yes | No (en/fr) | Voice cloning | CC-BY-4.0 | Architecture reference only |
| VibeVoice-Realtime 0.5B | Yes | Preset voices only | No cloning or emotion | MIT | Not controllable enough |
| Chatterbox Multilingual | No (forks chunk only) | Yes | Exaggeration, reference audio | MIT | Reference conditioning moved emotion (round 3), but RTF is 0.55 on the 3060 and it has no text streaming |
| Qwen3-TTS | Sentence-level only in released code | Yes | Instruct | Apache-2.0 | Slow here; bf16 only |

**Controls:**
- `SetPace` ramps over hundreds of milliseconds, never stepping.
- `SetStyle` takes an instruction or a reference-audio window.
- `Skip` and `Flush` drop or finish pending material.

**Telemetry:** `spoken_token_index`, buffered seconds, and first-audio latency.

### Playout

The playout stage keeps one continuous output clock per session. It crossfades segment joins, enforces a minimum inter-phrase gap, and applies pace changes as ramps. It reports which target tokens have actually been voiced, which is what lets the translator safely rewrite the unspoken backlog.

### Conductor

The conductor is a small deterministic policy service, with no model. Inputs:
- telemetry from every stage;
- the prosody stream.

Outputs:
- **Lag control.** Lag is the gap between the output clock and source time for the aligned content, with thresholds L1 < L2 < L3:
  - over L1, raise TTS pace slowly, capped at about 1.25×;
  - over L2, `Condense(0.7, scope=unspoken)` to the translator;
  - over L3, `Skip` to the next sentence boundary and resume.
- **Style.** Map smoothed prosody and emotion to `SetStyle` and `SetPace`, with hysteresis so delivery does not flap.
- **Context hygiene.** `Summarize` every N minutes or when the context passes a token budget, and seed the session glossary.
- **Recovery.** Watch for gaps and stalls; issue `Resync` or reassign the stage.

## Spike results (this round)

`e2e/bench/llm_streaming_translate.py`: Qwen3-4B-Instruct-2507 on the RTX 3060 in bf16 (Hugging Face `generate`, no serving optimizations), fed the 39 ASR fragments from the 90 s clip as conversational turns.

- **Continuity:** fragments that were split mid-thought come out as fluent continuations. "así que aquí está el" + "asunto"; "es por eso que nosotros" + "nos llamamos hermanos y hermanas en la fe"; mid-word "AP"/"Ostles" becomes "los apóst…". Compare opus-mt's "Así que aquí está el" + "Cosa" and "AP" + "Ostles poner el dinero".
- **Latency:** median 395 ms per fragment, p90 626 ms. KV-cache reuse saved only about 30 ms at this context size, because decoding dominates. An optimized server (llama.cpp or vLLM) should cut this substantially.
- **Condense per fragment does not work:** tagging each fragment `[CONDENSE]` produced *more* Spanish (467 vs 453 syllables). Fragments are already minimal.
- **Condense on a backlog works:** asking for "at most 60%" over four-fragment backlogs gave a median syllable ratio of 0.77 (range 0.73–0.92) in about 1.0 s per call. The main point survived and asides were dropped. Example: "…vendían bienes, vendían casas, vendían activos y traían el dinero a los líderes de esta comunidad" became "…vendían bienes, casas y activos, y entregaban el dinero a los líderes."

**Design consequence.** Condensing must act on the unspoken backlog, so the synthesizer must report what it has spoken and accept replacement of what it has not. That is the reason for the spoken watermark and the "speak only committed" rule.

## Latency budget (target)

| Step | Target |
| --- | --- |
| ASR commit after word end | 0.3–0.6 s (Zipformer partials or Kyutai 0.5 s) |
| Translator read policy (wait for enough source) | 0.8–1.5 s (clause or wait-k) |
| Translator decode | 0.1–0.3 s with a serving engine |
| TTS first audio | 0.15–0.3 s (CosyVoice 3 bi-streaming) |
| **Ear-to-voice span** | **~2–3 s**, versus 5–6 s today |

## Hardware placement

| Hardware | Role |
| --- | --- |
| RTX 3060 (bf16) | Translator LLM and CosyVoice 3 for the first phases. vLLM works, and both models fit in about 10 GB |
| V100 ×2 (fleet) | ASR (Nemotron lists Volta); LLM via llama.cpp in fp16; CosyVoice 3 only if fp16 holds up. Shared with audiobook jobs, so one long-lived reservation at most |
| Fleet CPU (node-1 and others) | Capture, prosody DSP, emotion2vec, conductor, playout, records, Zipformer ASR, Kokoro fallback |

## Migration plan

Every phase is gated by the existing real-graph evaluation. That evaluation gains a translation reference set and a discontinuity metric (abrupt onsets and clicks per minute).

| Phase | Work | Gate |
| --- | --- | --- |
| P0 | Add `Control` and `Telemetry` ports and contracts; conductor skeleton that only observes; playout with crossfades and ramps; ASR emits word deltas | No regressions; discontinuities drop to near zero |
| P1 | Streaming translator (Qwen3-4B via vLLM or llama.cpp) consuming committed words; glossary and summarize controls | Better translation on a referenced sermon set than opus-mt; translator latency p90 < 0.5 s |
| P2 | CosyVoice 3 bi-streaming TTS with spoken watermark, pace ramps, and style by instruction or reference audio; Kokoro remains a fallback stage | First-audio p90 < 0.4 s; carry-through at or above Kokoro's; emotion agreement above round 3 |
| P3 | Conductor policies: pace, condense-backlog, skip; summarize cadence | p90 ear-to-voice span < 3.5 s on the sermon with bounded drift over 30 minutes |
| P4 | ASR upgrade (Kyutai STT 1B or Nemotron) with end-of-turn signals | Lower WER and commit delay than Zipformer |

The direct-model track (Hibiki-style fine-tune) stays separate. Its natural place is as a single stage that consumes audio plus control and emits audio plus telemetry under the same contracts.

## Open questions

- Translation quality reference: a human Spanish rendering of a few sermon minutes would let us score context-aware vs fragment translation directly.
- Whether tentative ASR words should drive speculative translation (lower lag, more retraction) or only committed words (simpler; this proposal's default).
- CosyVoice 3 on Volta in fp16, and its real first-audio latency with incremental text on the 3060.
- License review for Nemotron (NVIDIA Open Model License) and Seed-X (OpenMDW) before adoption.

## Implementation status

**Implemented:**
- **Contracts:** `ControlPayload` (condense, summarize, glossary), `TelemetryPayload`, `TranslationPayload.replaces`, and the translator processor protocol (`TranslatorRequest` / `TranslatorResponse`).
- **Channels:** `graph.translate.control` and `graph.telemetry`.
- **ASR:** commits only whole words, at clause boundaries or once a phrase reaches 3.5 s (everything but the last two words). Punctuation uses the previous six committed words as context, so continuations keep lower case. The recognizer's hard cut is now only a 15 s safety net. Mid-word splits such as "AP"/"Ostles" and "univer"/"Sal" are gone; one remains at a silence endpoint ("distribut"/"E").
- **Prosody:** publishes the decoded source clock every 250 ms.
- **Streaming translator:** `--translator=llm`, running Qwen3-4B-Instruct on the RTX 3060.
  - It keeps a conversation, a running summary, and a glossary per session.
  - Each committed fragment gets the Spanish that continues the previous output; a fragment may be held until more arrives.
  - It accepts condense, summarize, and glossary controls and reports context size.
- **TTS:**
  - Phrases are held until their slot is within 2 s of the source clock; held phrases are revisable through `replaces`, and stale replacements are dropped.
  - Speed, pitch, level, range, and arousal are smoothed between phrases.
  - Phrase edges get 30 ms fades, with at least 120 ms between phrases.
  - Kokoro resamples each phrase in one piece.
  - Telemetry reports the queue, backlog, and projected lag.
- **Conductor:**
  - It asks for condensing of the queued backlog beyond the next two phrases when projected lag exceeds 6 s, with a 1.5 s cooldown.
  - It asks for a context summary when the translator holds 16 turns.
- **Evaluation:** `qol-eval continuity`; the real graph test reports phrase joins and onsets, and can stream the input in real time (`QOL_E2E_REALTIME=1`) so the clock and backlog behave as live.

Results on the 90 s sermon clip, Kokoro on CPU:

| Configuration | Rate r | Level r | Pitch r | Range r | Onset lag median / p90 | Shortest phrase gap | Controls |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Line translator, no min gap | 0.72 | 0.87 | 0.80 | 0.73 | 5.08 s / 6.13 s | 199 ms | none |
| Line translator, defaults | 0.68 | 0.86 | 0.81 | 0.70 | 5.51 s / 6.61 s | 314 ms | none |
| LLM translator plus conductor, real-time input | 0.66 | 0.90 | 0.81 | 0.74 | 5.12 s / 6.30 s | 319 ms | 3 summarize |
| LLM translator plus conductor, real-time input, earlier tuning (protect 1 phrase) | 0.72 | 0.88 | 0.81 | 0.79 | 5.10 s / 6.34 s | n/a | 2 condense (1 applied, 1 too late), 7 summarize |

Observations:
- **Translation:** the translator continues across fragments faithfully once told never to finish the speaker's sentence; an earlier prompt invented clauses. Per-fragment latency is 0.2–0.8 s on the 3060 with Hugging Face `generate`.
- **Condensing:** it fires rarely on this clip, because lag stays near 5–6 s. Condensing a held backlog produced fluent but only modestly shorter Spanish in the probe (about 0.9 of the original), against 0.77 in the standalone spike.
- **Phrase joins:** gaps between phrases were never under 80 ms, even without the minimum gap, because Kokoro pads each phrase.
- **What remains audible:** Kokoro starts each phrase from digital silence (5–8 onsets per minute by `qol-eval continuity`, against 0 for natural English). Each phrase still gets its own intonation contour. Both need a TTS that streams text across phrases (P2).

**Not yet implemented:**
- **P2:** CosyVoice 3 bi-streaming TTS with a spoken watermark.
- **Serving engine:** llama.cpp or vLLM with prompt caching for the translator.
- **Conductor:** skip and pace policies.
- **P4:** ASR upgrade.
- **Fleet:** `qol-graph` still runs the earlier images with the line translator.

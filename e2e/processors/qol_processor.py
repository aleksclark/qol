from __future__ import annotations

import os
import sys
from typing import BinaryIO

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "gen"))

from qol.v1 import processor_pb2, qol_pb2

MAX_FRAME_SIZE = 4 << 20
OUTPUT_SAMPLE_RATE = 22050
CHUNK_BYTES = 64 * 1024


def claim_stdout() -> BinaryIO:
    sys.stdout.flush()
    frames = os.fdopen(os.dup(1), "wb")
    os.dup2(2, 1)
    return frames


def _read_varint(stream: BinaryIO) -> int | None:
    shift = 0
    result = 0
    while True:
        byte = stream.read(1)
        if not byte:
            if shift == 0:
                return None
            raise EOFError("truncated frame length")
        value = byte[0]
        result |= (value & 0x7F) << shift
        if not value & 0x80:
            return result
        shift += 7
        if shift > 63:
            raise ValueError("frame length overflows")


def _write_varint(stream: BinaryIO, value: int) -> None:
    while True:
        byte = value & 0x7F
        value >>= 7
        if value:
            stream.write(bytes([byte | 0x80]))
        else:
            stream.write(bytes([byte]))
            return


def read_frame(stream: BinaryIO, message):
    size = _read_varint(stream)
    if size is None:
        return None
    if size > MAX_FRAME_SIZE:
        raise ValueError(f"frame exceeds {MAX_FRAME_SIZE} bytes")
    payload = stream.read(size)
    if len(payload) != size:
        raise EOFError("truncated frame")
    message.ParseFromString(payload)
    return message


def write_frame(stream: BinaryIO, message) -> None:
    payload = message.SerializeToString()
    if len(payload) > MAX_FRAME_SIZE:
        raise ValueError(f"frame exceeds {MAX_FRAME_SIZE} bytes")
    _write_varint(stream, len(payload))
    stream.write(payload)


def read_synthesis_requests(stream: BinaryIO):
    while True:
        request = read_frame(stream, processor_pb2.SynthesisRequest())
        if request is None:
            return
        yield request


def write_utterance(stream: BinaryIO, start_ns: int, end_ns: int, text: str) -> None:
    write_frame(
        stream,
        processor_pb2.RecognizedUtterance(
            span=qol_pb2.MediaSpan(start_ns=start_ns, end_ns=end_ns), text=text
        ),
    )
    stream.flush()


def write_pcm(stream: BinaryIO, pcm: bytes, end: bool = True, sample_rate: int = OUTPUT_SAMPLE_RATE) -> None:
    pcm_format = qol_pb2.PcmFormat(
        sample_rate_hz=sample_rate,
        channel_count=1,
        sample_format=qol_pb2.PCM_SAMPLE_FORMAT_S16_LE,
    )
    if not pcm and not end:
        return
    blocks = [pcm[offset : offset + CHUNK_BYTES] for offset in range(0, len(pcm), CHUNK_BYTES)] or [b""]
    for index, block in enumerate(blocks):
        write_frame(
            stream,
            processor_pb2.SynthesisChunk(
                format=pcm_format,
                data=block,
                end_of_utterance=end and index == len(blocks) - 1,
            ),
        )
    stream.flush()


def clamp(value: float, low: float, high: float) -> float:
    return max(low, min(high, value))


def speaking_speed(prosody, low: float = 0.7, high: float = 1.6) -> float:
    if prosody is None or prosody.relative_rate <= 0:
        return 1.0
    return clamp(prosody.relative_rate, low, high)


def loudness_gain(prosody) -> float:
    if prosody is None:
        return 1.0
    return 10 ** (clamp(prosody.relative_energy_db, -10, 8) / 20)


def describe_delivery(prosody) -> str:
    if prosody is None or not prosody.ByteSize():
        return "Speak in a natural, neutral conversational tone."
    parts: list[str] = []
    rate = prosody.relative_rate or 1.0
    if rate >= 1.4:
        parts.append("at a very fast pace")
    elif rate >= 1.15:
        parts.append("at a brisk, fast pace")
    elif rate <= 0.7:
        parts.append("at a very slow, deliberate pace")
    elif rate <= 0.87:
        parts.append("at a slow, measured pace")
    if prosody.relative_energy_db >= 4:
        parts.append("with a loud, projecting voice")
    elif prosody.relative_energy_db <= -4:
        parts.append("with a soft, quiet voice")
    if prosody.relative_pitch_semitones >= 2:
        parts.append("in a raised pitch")
    elif prosody.relative_pitch_semitones <= -2:
        parts.append("in a lowered pitch")
    if prosody.pitch_range_semitones >= 9:
        parts.append("with lively, animated intonation")
    elif 0 < prosody.pitch_range_semitones <= 3:
        parts.append("with flat, even intonation")
    mood = ""
    if prosody.emotion and prosody.emotion_confidence >= 0.5:
        mood = f"sounding {prosody.emotion}"
    elif prosody.arousal >= 0.7:
        mood = "sounding excited and intense"
    elif 0 < prosody.arousal <= 0.25:
        mood = "sounding calm and relaxed"
    if mood:
        parts.append(mood)
    if prosody.emphasis >= 0.6:
        parts.append("stressing key words emphatically")
    if not parts:
        return "Speak in a natural, neutral conversational tone."
    return "Speak " + ", ".join(parts) + "."

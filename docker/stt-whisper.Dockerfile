FROM golang:1.26.6-bookworm

RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg python3 python3-pip curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN pip3 install --break-system-packages --no-cache-dir sherpa-onnx numpy
RUN go install github.com/air-verse/air@v1.63.6

ENV QOL_PROCESSOR=/usr/bin/python3
ENV QOL_PROCESSOR_ARG=/processors/stt_whisper.py
ENV QOL_SHERPA_MODEL=/models/sherpa-onnx-streaming-zipformer-en-2023-06-26

COPY e2e/processors/stt_whisper.py /processors/stt_whisper.py
RUN apt-get update \
    && apt-get install -y --no-install-recommends bzip2 \
    && mkdir -p /models \
    && curl -fsSL https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-streaming-zipformer-en-2023-06-26.tar.bz2 \
    | tar -xjf - -C /models \
    && apt-get purge -y bzip2 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

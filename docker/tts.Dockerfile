FROM golang:1.26.6-bookworm

RUN apt-get update \
    && apt-get install -y --no-install-recommends espeak python3 \
    && rm -rf /var/lib/apt/lists/*
RUN go install github.com/air-verse/air@v1.63.6

ENV QOL_PROCESSOR=/usr/bin/python3
ENV QOL_PROCESSOR_ARG=/processors/tts_spanish.py

COPY e2e/processors/tts_spanish.py /processors/tts_spanish.py

WORKDIR /app

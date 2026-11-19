FROM golang:1.26.6-bookworm

RUN apt-get update \
    && apt-get install -y --no-install-recommends espeak-ng python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*
RUN pip3 install --break-system-packages --no-cache-dir torch==2.8.0 --extra-index-url https://download.pytorch.org/whl/cpu \
    && pip3 install --break-system-packages --no-cache-dir kokoro==0.9.4 'misaki[en]==0.9.4' scipy 'protobuf>=7.36.1,<8' huggingface-hub
RUN go install github.com/air-verse/air@v1.63.6

ENV QOL_PROCESSOR=/usr/bin/python3
ENV QOL_PROCESSOR_ARG=/processors/tts_kokoro.py
ENV QOL_KOKORO_DEVICE=cpu
ENV QOL_KOKORO_VOICE=em_alex
ENV HF_HOME=/models/huggingface

RUN python3 -c "from kokoro import KPipeline; pipeline = KPipeline(lang_code='e', repo_id='hexgrad/Kokoro-82M', device='cpu'); [r for r in pipeline('Hola a todos.', voice='em_alex')]; [r for r in pipeline('Hola a todos.', voice='ef_dora')]"
ENV HF_HUB_OFFLINE=1

COPY e2e/processors /processors

WORKDIR /app

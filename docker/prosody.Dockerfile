FROM golang:1.26.6-bookworm

RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*
RUN pip3 install --break-system-packages --no-cache-dir torch==2.8.0 torchaudio==2.8.0 --extra-index-url https://download.pytorch.org/whl/cpu \
    && pip3 install --break-system-packages --no-cache-dir funasr==1.4.27 huggingface-hub==0.36.0 numpy 'protobuf>=7.36.1,<8'
RUN go install github.com/air-verse/air@v1.63.6

ENV QOL_PROCESSOR=/usr/bin/python3
ENV QOL_PROCESSOR_ARG=/processors/emotion_emotion2vec.py
ENV QOL_EMOTION_MODEL=emotion2vec/emotion2vec_plus_base
ENV HF_HOME=/models/huggingface

RUN python3 -c "from funasr import AutoModel; AutoModel(model='emotion2vec/emotion2vec_plus_base', hub='hf', disable_update=True)"
ENV HF_HUB_OFFLINE=1

COPY e2e/processors /processors

WORKDIR /app

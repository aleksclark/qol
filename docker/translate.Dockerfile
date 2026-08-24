FROM golang:1.26.6-bookworm

RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*
RUN pip3 install --break-system-packages --no-cache-dir torch==2.8.0 --extra-index-url https://download.pytorch.org/whl/cpu \
    && pip3 install --break-system-packages --no-cache-dir ctranslate2==4.8.1 huggingface-hub==0.36.0 sentencepiece==0.2.1 transformers==4.57.6
RUN go install github.com/air-verse/air@v1.63.6

ENV QOL_PROCESSOR=/usr/bin/python3
ENV QOL_PROCESSOR_ARG=/processors/translate_en_es.py
ENV QOL_TRANSLATE_MODEL=/models/opus-mt-en-es-ct2
ENV HF_HOME=/models/huggingface

COPY e2e/processors/translate_en_es.py /processors/translate_en_es.py
RUN python3 -c "from huggingface_hub import snapshot_download; snapshot_download('Helsinki-NLP/opus-mt-en-es')" \
    && model=$(find /models/huggingface -type d -path '*/snapshots/*' | head -n 1) \
    && test -n "$model" \
    && ct2-transformers-converter --model "$model" --output_dir /models/opus-mt-en-es-ct2 --quantization int8
ENV HF_HUB_OFFLINE=1

WORKDIR /app

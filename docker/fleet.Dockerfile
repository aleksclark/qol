FROM golang:1.26.6-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/qol-worker ./cmd/qol-worker

FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ffmpeg \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/qol-worker /usr/local/bin/qol-worker
ENTRYPOINT ["qol-worker", "run"]

FROM runtime AS go-worker

FROM runtime AS python
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-pip curl bzip2 \
    && rm -rf /var/lib/apt/lists/*
ENV QOL_PROCESSOR=/usr/bin/python3
ENV HF_HOME=/models/huggingface

FROM python AS stt
RUN pip3 install --break-system-packages --no-cache-dir sherpa-onnx==2.0.2 numpy 'protobuf>=7.36.1,<8'
RUN mkdir -p /models \
    && curl -fsSL https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-streaming-zipformer-en-2023-06-26.tar.bz2 | tar -xjf - -C /models \
    && curl -fsSL https://github.com/k2-fsa/sherpa-onnx/releases/download/punctuation-models/sherpa-onnx-online-punct-en-2024-08-06.tar.bz2 | tar -xjf - -C /models
ENV QOL_SHERPA_MODEL=/models/sherpa-onnx-streaming-zipformer-en-2023-06-26
ENV QOL_PUNCT_MODEL=/models/sherpa-onnx-online-punct-en-2024-08-06
ENV QOL_PROCESSOR_ARG=/processors/stt_whisper.py
COPY e2e/processors /processors

FROM python AS translate
RUN pip3 install --break-system-packages --no-cache-dir torch==2.8.0 --extra-index-url https://download.pytorch.org/whl/cpu \
    && pip3 install --break-system-packages --no-cache-dir ctranslate2==4.8.1 huggingface-hub==0.36.0 sentencepiece==0.2.1 transformers==4.57.6 sacremoses==0.1.1
RUN model=$(python3 -c "from huggingface_hub import snapshot_download; print(snapshot_download('Helsinki-NLP/opus-mt-tc-big-en-es'))") \
    && ct2-transformers-converter --model "$model" --output_dir /models/opus-mt-tc-big-en-es-ct2 --quantization int8
ENV QOL_TRANSLATE_MODEL=/models/opus-mt-tc-big-en-es-ct2
ENV QOL_TRANSLATE_HF_MODEL=Helsinki-NLP/opus-mt-tc-big-en-es
ENV HF_HUB_OFFLINE=1
ENV QOL_PROCESSOR_ARG=/processors/translate_en_es.py
COPY e2e/processors /processors

FROM python AS tts
RUN apt-get update \
    && apt-get install -y --no-install-recommends espeak-ng \
    && rm -rf /var/lib/apt/lists/*
RUN pip3 install --break-system-packages --no-cache-dir torch==2.8.0 --extra-index-url https://download.pytorch.org/whl/cpu \
    && pip3 install --break-system-packages --no-cache-dir kokoro==0.9.4 'misaki[en]==0.9.4' scipy 'protobuf>=7.36.1,<8' huggingface-hub
RUN python3 -c "from kokoro import KPipeline; pipeline = KPipeline(lang_code='e', repo_id='hexgrad/Kokoro-82M', device='cpu'); [r for r in pipeline('Hola a todos.', voice='em_alex')]; [r for r in pipeline('Hola a todos.', voice='ef_dora')]"
ENV HF_HUB_OFFLINE=1
ENV QOL_KOKORO_DEVICE=cpu
ENV QOL_KOKORO_VOICE=em_alex
ENV QOL_PROCESSOR_ARG=/processors/tts_kokoro.py
COPY e2e/processors /processors

FROM python AS prosody
RUN pip3 install --break-system-packages --no-cache-dir torch==2.8.0 torchaudio==2.8.0 --extra-index-url https://download.pytorch.org/whl/cpu \
    && pip3 install --break-system-packages --no-cache-dir funasr==1.4.27 huggingface-hub==0.36.0 numpy 'protobuf>=7.36.1,<8'
RUN python3 -c "from funasr import AutoModel; AutoModel(model='emotion2vec/emotion2vec_plus_base', hub='hf', disable_update=True)"
ENV HF_HUB_OFFLINE=1
ENV QOL_EMOTION_MODEL=emotion2vec/emotion2vec_plus_base
ENV QOL_PROCESSOR_ARG=/processors/emotion_emotion2vec.py
COPY e2e/processors /processors

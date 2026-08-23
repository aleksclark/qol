FROM golang:1.26.6-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg && rm -rf /var/lib/apt/lists/*
RUN go install github.com/air-verse/air@v1.63.6
WORKDIR /app

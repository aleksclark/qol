FROM golang:1.26.6-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG COMMAND
RUN CGO_ENABLED=0 go build -o /out/service ./cmd/${COMMAND}

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates ffmpeg && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/service /usr/local/bin/service
ENTRYPOINT ["service"]

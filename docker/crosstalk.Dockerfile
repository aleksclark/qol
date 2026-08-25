FROM golang:1.26.6-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/qol-worker ./cmd/qol-worker

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates ffmpeg \
	&& rm -rf /var/lib/apt/lists/* \
	&& groupadd --system --gid 65532 nonroot \
	&& useradd --system --uid 65532 --gid 65532 --home /tmp --shell /usr/sbin/nologin nonroot
COPY --from=build /out/qol-worker /usr/local/bin/qol-worker
USER 65532:65532
WORKDIR /tmp
ENV QOL_HEALTH_FILE=/tmp/qol-crosstalk-health
HEALTHCHECK --interval=5s --timeout=2s --retries=12 CMD ["qol-worker", "health"]
ENTRYPOINT ["qol-worker"]

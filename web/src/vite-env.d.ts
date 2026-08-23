/// <reference types="vite/client" />

interface WebTransportBidirectionalStream {
  readable: ReadableStream<Uint8Array>
  writable: WritableStream<Uint8Array>
}

interface WebTransportHash {
  algorithm: "sha-256"
  value: BufferSource
}

interface WebTransportOptions {
  serverCertificateHashes?: WebTransportHash[]
}

interface WebTransport {
  readonly ready: Promise<void>
  readonly closed: Promise<void>
  readonly incomingBidirectionalStreams: ReadableStream<WebTransportBidirectionalStream>
  createBidirectionalStream(): Promise<WebTransportBidirectionalStream>
  close(options?: { closeCode?: number; reason?: string }): void
}

declare const WebTransport: {
  prototype: WebTransport
  new(url: string, options?: WebTransportOptions): WebTransport
}

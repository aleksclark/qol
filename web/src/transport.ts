import { create, fromBinary, toBinary } from "@bufbuild/protobuf"
import {
  AudioChunkSchema,
  ClientFrameSchema,
  ServerFrameSchema,
  StartUploadSchema,
  type ServerFrame,
} from "./gen/qol/v1/qol_pb"

export type AudioSource = {
  name: string
  mediaType: string
  sizeBytes?: bigint
  chunks: AsyncIterable<Uint8Array>
}

function encodeVarint(value: number): Uint8Array {
  const bytes: number[] = []
  do {
    let byte = value & 0x7f
    value >>>= 7
    if (value > 0) byte |= 0x80
    bytes.push(byte)
  } while (value > 0)
  return Uint8Array.from(bytes)
}

function frame(payload: Uint8Array): Uint8Array {
  const prefix = encodeVarint(payload.length)
  const result = new Uint8Array(prefix.length + payload.length)
  result.set(prefix)
  result.set(payload, prefix.length)
  return result
}

class FrameReader {
  private buffered = new Uint8Array()

  push(chunk: Uint8Array): ServerFrame[] {
    const joined = new Uint8Array(this.buffered.length + chunk.length)
    joined.set(this.buffered)
    joined.set(chunk, this.buffered.length)
    this.buffered = joined
    const messages: ServerFrame[] = []
    while (this.buffered.length) {
      let size = 0
      let shift = 0
      let prefix = 0
      for (; prefix < this.buffered.length; prefix++) {
        const byte = this.buffered[prefix]
        size |= (byte & 0x7f) << shift
        if ((byte & 0x80) === 0) break
        shift += 7
      }
      if (prefix === this.buffered.length || this.buffered.length < prefix + 1 + size) break
      const start = prefix + 1
      messages.push(fromBinary(ServerFrameSchema, this.buffered.slice(start, start + size)))
      this.buffered = this.buffered.slice(start + size)
    }
    return messages
  }
}

export async function streamAudio(url: string, source: AudioSource, onFrame: (message: ServerFrame) => void): Promise<void> {
  const encodedHash = import.meta.env.VITE_WEBTRANSPORT_CERT_HASH
  const options = encodedHash ? { serverCertificateHashes: [{ algorithm: "sha-256" as const, value: Uint8Array.from(atob(encodedHash), (character) => character.charCodeAt(0)) }] } : undefined
  const transport = new WebTransport(url, options)
  try {
    await transport.ready
    const stream = await transport.createBidirectionalStream()
    const writer = stream.writable.getWriter()
    const reader = stream.readable.getReader()
    const start = create(ClientFrameSchema, { body: { case: "startUpload", value: create(StartUploadSchema, { name: source.name, sizeBytes: source.sizeBytes ?? 0n, mediaType: source.mediaType }) } })
    await writer.write(frame(toBinary(ClientFrameSchema, start)))
    const response = (async () => {
      const frames = new FrameReader()
      for (;;) {
        const { value, done } = await reader.read()
        if (done) return
        for (const message of frames.push(value)) onFrame(message)
      }
    })()
    let sequence = 0n
    for await (const data of source.chunks) {
      if (data.length === 0) continue
      const chunk = create(ClientFrameSchema, { body: { case: "audioChunk", value: create(AudioChunkSchema, { sequence, data }) } })
      await writer.write(frame(toBinary(ClientFrameSchema, chunk)))
      sequence++
    }
    const end = create(ClientFrameSchema, { body: { case: "audioChunk", value: create(AudioChunkSchema, { sequence, endOfStream: true }) } })
    await writer.write(frame(toBinary(ClientFrameSchema, end)))
    await writer.close()
    await response
  } finally {
    transport.close()
  }
}

export async function streamFile(url: string, file: File, onFrame: (message: ServerFrame) => void): Promise<void> {
  const chunkSize = 64 * 1024
  async function* chunks() {
    for (let offset = 0; offset < file.size; offset += chunkSize) {
      yield new Uint8Array(await file.slice(offset, offset + chunkSize).arrayBuffer())
    }
  }
  return streamAudio(url, { name: file.name, mediaType: file.type || "application/octet-stream", sizeBytes: BigInt(file.size), chunks: chunks() }, onFrame)
}

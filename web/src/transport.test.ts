import { create, toBinary } from "@bufbuild/protobuf"
import { describe, expect, it, vi } from "vitest"
import { ClientFrameSchema, ServerFrameSchema } from "./gen/qol/v1/qol_pb"
import { fromBinary } from "@bufbuild/protobuf"
import { streamAudio, streamFile } from "./transport"

function framed(payload: Uint8Array): Uint8Array {
  return Uint8Array.from([payload.length, ...payload])
}

function unframe(payload: Uint8Array) {
  return fromBinary(ClientFrameSchema, payload.slice(1))
}

describe("audio streaming", () => {
  it("sends an audio file and reads framed server events", async () => {
    const accepted = create(ServerFrameSchema, { body: { case: "uploadAccepted", value: { sessionId: "session" } } })
    const completed = create(ServerFrameSchema, { body: { case: "uploadCompleted", value: { sessionId: "session", status: "complete", outputPath: "/output/session.ogg" } } })
    const received = [framed(toBinary(ServerFrameSchema, accepted)), framed(toBinary(ServerFrameSchema, completed))]
    const writes: Uint8Array[] = []
    class MockTransport {
      ready = Promise.resolve()
      closed = Promise.resolve()
      incomingBidirectionalStreams = new ReadableStream()
      async createBidirectionalStream() {
        return {
          readable: new ReadableStream<Uint8Array>({ start(controller) { received.forEach((value) => controller.enqueue(value)); controller.close() } }),
          writable: new WritableStream<Uint8Array>({ write(value) { writes.push(value) } }),
        }
      }
      close() {}
    }
    vi.stubGlobal("WebTransport", MockTransport)
    const cases: string[] = []
    await streamFile("https://example.test", new File([new Uint8Array([1, 2, 3])], "voice.mp3", { type: "audio/mpeg" }), (message) => cases.push(message.body.case ?? ""))
    expect(cases).toEqual(["uploadAccepted", "uploadCompleted"])
    expect(writes).toHaveLength(3)
  })

  it("streams live chunks until the source ends", async () => {
    const accepted = create(ServerFrameSchema, { body: { case: "uploadAccepted", value: { sessionId: "live" } } })
    const completed = create(ServerFrameSchema, { body: { case: "uploadCompleted", value: { sessionId: "live", status: "complete", outputPath: "/output/live.ogg" } } })
    const writes: Uint8Array[] = []
    class MockTransport {
      ready = Promise.resolve()
      closed = Promise.resolve()
      incomingBidirectionalStreams = new ReadableStream()
      async createBidirectionalStream() {
        return {
          readable: new ReadableStream<Uint8Array>({ start(controller) { controller.enqueue(framed(toBinary(ServerFrameSchema, accepted))); controller.enqueue(framed(toBinary(ServerFrameSchema, completed))); controller.close() } }),
          writable: new WritableStream<Uint8Array>({ write(value) { writes.push(value) } }),
        }
      }
      close() {}
    }
    vi.stubGlobal("WebTransport", MockTransport)
    async function* chunks() { yield Uint8Array.from([1]); yield Uint8Array.from([2, 3]) }
    await streamAudio("https://example.test", { name: "microphone.webm", mediaType: "audio/webm;codecs=opus", chunks: chunks() }, () => undefined)
    expect(unframe(writes[0]).body.value).toMatchObject({ name: "microphone.webm", mediaType: "audio/webm;codecs=opus", sizeBytes: 0n })
    expect(writes.slice(1).map((write) => unframe(write).body.case)).toEqual(["audioChunk", "audioChunk", "audioChunk"])
    expect(unframe(writes[3]).body.value).toMatchObject({ sequence: 2n, endOfStream: true })
  })
})

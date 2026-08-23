import { create, toBinary } from "@bufbuild/protobuf"
import { describe, expect, it, vi } from "vitest"
import { ServerFrameSchema } from "./gen/qol/v1/qol_pb"
import { streamFile } from "./transport"

function framed(payload: Uint8Array): Uint8Array {
  return Uint8Array.from([payload.length, ...payload])
}

describe("streamFile", () => {
  it("sends an MP3 and reads framed server events", async () => {
    const accepted = create(ServerFrameSchema, { body: { case: "uploadAccepted", value: { sessionId: "session" } } })
    const completed = create(ServerFrameSchema, { body: { case: "uploadCompleted", value: { inputEvents: 1n, outputEvents: 1n } } })
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
})

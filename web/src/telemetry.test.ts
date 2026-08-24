import { describe, expect, it } from "vitest"
import { appendTopicSample } from "./telemetry"
import type { TopicActivity } from "./gen/qol/v1/qol_pb"

function topic(subject: string, eventCount: bigint): TopicActivity {
  return { $typeName: "qol.v1.TopicActivity", subject, eventCount }
}

describe("appendTopicSample", () => {
  it("records event deltas and keeps the latest samples", () => {
    const initial = appendTopicSample([], [topic("qol.graph.stt-whisper.input", 10n)], 2)
    const second = appendTopicSample(initial, [topic("qol.graph.stt-whisper.input", 13n)], 2)
    const third = appendTopicSample(second, [topic("qol.graph.stt-whisper.input", 18n)], 2)

    expect(third).toEqual([{
      subject: "qol.graph.stt-whisper.input",
      total: 18n,
      samples: [3, 5],
    }])
  })

  it("treats a reset counter as new activity", () => {
    const initial = appendTopicSample([], [topic("qol.graph.record-es.completed", 8n)])
    const reset = appendTopicSample(initial, [topic("qol.graph.record-es.completed", 2n)])

    expect(reset[0].samples.at(-1)).toBe(2)
  })
})

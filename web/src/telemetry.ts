import type { TopicActivity } from "./gen/qol/v1/qol_pb"

export type TopicHistory = {
  subject: string
  total: bigint
  samples: number[]
}

export function appendTopicSample(current: TopicHistory[], topics: TopicActivity[], limit = 36): TopicHistory[] {
  const previous = new Map(current.map((topic) => [topic.subject, topic]))
  return topics.map((topic) => {
    const existing = previous.get(topic.subject)
    const delta = existing ? Number(topic.eventCount >= existing.total ? topic.eventCount - existing.total : topic.eventCount) : 0
    return {
      subject: topic.subject,
      total: topic.eventCount,
      samples: [...(existing?.samples ?? []), delta].slice(-limit),
    }
  })
}

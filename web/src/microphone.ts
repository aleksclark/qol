import type { AudioSource } from "./transport"

export type MicrophoneCapture = {
  source: AudioSource
  stop: () => void
}

export async function captureMicrophone(): Promise<MicrophoneCapture> {
  if (!navigator.mediaDevices?.getUserMedia || typeof MediaRecorder === "undefined") {
    throw new Error("Microphone capture is not supported by this browser")
  }
  const stream = await navigator.mediaDevices.getUserMedia({ audio: true })
  const mediaType = selectMediaType()
  const recorder = mediaType ? new MediaRecorder(stream, { mimeType: mediaType }) : new MediaRecorder(stream)
  const chunks: Blob[] = []
  let resolveChunk: (() => void) | undefined
  let stopped = false
  let stopRequested = false
  recorder.addEventListener("dataavailable", (event) => {
    if (event.data.size > 0) chunks.push(event.data)
    resolveChunk?.()
    resolveChunk = undefined
  })
  recorder.addEventListener("stop", () => {
    stopped = true
    resolveChunk?.()
    resolveChunk = undefined
    stream.getTracks().forEach((track) => track.stop())
  })
  async function* audioChunks() {
    recorder.start(1000)
    if (stopRequested) recorder.stop()
    try {
      for (;;) {
        if (chunks.length > 0) {
          yield new Uint8Array(await chunks.shift()!.arrayBuffer())
          continue
        }
        if (stopped) return
        await new Promise<void>((resolve) => { resolveChunk = resolve })
      }
    } finally {
      if (recorder.state !== "inactive") recorder.stop()
      stream.getTracks().forEach((track) => track.stop())
    }
  }
  return {
    source: { name: `microphone-${new Date().toISOString()}.webm`, mediaType: recorder.mimeType || mediaType || "audio/webm", chunks: audioChunks() },
    stop: () => {
      stopRequested = true
      if (recorder.state === "recording" || recorder.state === "paused") recorder.stop()
    },
  }
}

function selectMediaType(): string {
  return ["audio/webm;codecs=opus", "audio/ogg;codecs=opus", "audio/webm"].find((mediaType) => MediaRecorder.isTypeSupported(mediaType)) ?? ""
}

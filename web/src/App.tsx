import { useEffect, useState, type FormEvent } from "react"
import { currentUser, login, logout, topicActivity, uploadTicket, webTransportURL } from "./api"
import { captureMicrophone, type MicrophoneCapture } from "./microphone"
import { appendTopicSample, type TopicHistory } from "./telemetry"
import { streamAudio, streamFile } from "./transport"
import type { User } from "./gen/qol/v1/qol_pb"

export default function App() {
  const [user, setUser] = useState<User | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState("")

  useEffect(() => {
    currentUser().then(setUser).catch(() => undefined).finally(() => setLoading(false))
  }, [])

  if (loading) return <main className="center-state"><span className="status info">Checking session</span></main>
  if (!user) return <Login onLogin={setUser} error={error} setError={setError} />
  return <Console user={user} onLogout={async () => { await logout(); setUser(null) }} />
}

function Login({ onLogin, error, setError }: { onLogin: (user: User) => void; error: string; setError: (message: string) => void }) {
  const [busy, setBusy] = useState(false)
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setBusy(true)
    setError("")
    const data = new FormData(event.currentTarget)
    try {
      onLogin(await login(String(data.get("username")), String(data.get("password"))))
    } catch (problem) {
      setError(problem instanceof Error ? problem.message : "Login failed")
    } finally {
      setBusy(false)
    }
  }
  return <main className="login-layout">
    <section className="login-intro">
      <span className="eyebrow">Voice systems</span>
      <h1>Qol</h1>
      <p>Observe voice processing as it moves through the system.</p>
    </section>
    <form className="login-form" onSubmit={submit}>
      <div className="form-heading"><span className="eyebrow">Administrator access</span><h2>Sign in</h2></div>
      <label><span>Username</span><input name="username" autoComplete="username" required /></label>
      <label><span>Password</span><input name="password" type="password" autoComplete="current-password" required /></label>
      {error && <p className="form-error" role="alert">{error}</p>}
      <button className="primary" disabled={busy}>{busy ? "Signing in" : "Sign in"}</button>
    </form>
  </main>
}

function Console({ user, onLogout }: { user: User; onLogout: () => Promise<void> }) {
  const [file, setFile] = useState<File | null>(null)
  const [source, setSource] = useState<"file" | "microphone">("file")
  const [capture, setCapture] = useState<MicrophoneCapture | null>(null)
  const [state, setState] = useState("Ready")
  const [session, setSession] = useState("")
  const [outputPaths, setOutputPaths] = useState<string[]>([])
  const [error, setError] = useState("")
  const [topicError, setTopicError] = useState("")
  const [topics, setTopics] = useState<TopicHistory[]>([])
  const [observedAt, setObservedAt] = useState(0)

  useEffect(() => {
    const controller = new AbortController()
    let stopped = false
    async function sample() {
      try {
        const activity = await topicActivity(controller.signal)
        if (stopped) return
        setTopics((current) => appendTopicSample(current, activity.topics))
        setObservedAt(Number(activity.observedAtUnixMilli))
        setTopicError("")
      } catch (problem) {
        if (!stopped && !(problem instanceof DOMException && problem.name === "AbortError")) {
          setTopicError(problem instanceof Error ? problem.message : "Topic activity is unavailable")
        }
      }
    }
    void sample()
    const interval = window.setInterval(() => void sample(), 1000)
    return () => {
      stopped = true
      controller.abort()
      window.clearInterval(interval)
    }
  }, [])

  function onFrame(frame: Parameters<typeof streamFile>[2] extends (message: infer Message) => void ? Message : never) {
    if (frame.body.case === "uploadAccepted") {
      setSession(frame.body.value.sessionId)
      setState(source === "microphone" ? "Recording" : "Streaming")
    }
    if (frame.body.case === "uploadCompleted") {
      const paths = frame.body.value.outputPaths.length > 0 ? frame.body.value.outputPaths : [frame.body.value.outputPath].filter(Boolean)
      setOutputPaths(paths)
      setState("Complete")
    }
    if (frame.body.case === "error") throw new Error(frame.body.value.message)
  }

  async function start() {
    if (source === "file" && !file) return
    setState(source === "microphone" ? "Requesting microphone" : "Connecting")
    setError("")
    setOutputPaths([])
    try {
      const ticket = await uploadTicket()
      if (source === "microphone") {
        const microphone = await captureMicrophone()
        setCapture(microphone)
        await streamAudio(webTransportURL(ticket), microphone.source, onFrame)
        setCapture(null)
      } else if (file) {
        await streamFile(webTransportURL(ticket), file, onFrame)
      }
    } catch (problem) {
      setCapture(null)
      setState("Failed")
      setError(problem instanceof Error ? problem.message : "Upload failed")
    }
  }

  return <div className="shell">
    <nav>
      <div className="wordmark">Qol</div>
      <div className="nav-item active"><span className="nav-icon" aria-hidden="true">●</span>Topic activity</div>
      <div className="account"><span>{user.username}</span><small>Administrator</small><button className="ghost" onClick={onLogout}>Sign out</button></div>
    </nav>
    <main className="workspace">
      <header><div><span className="eyebrow">NATS · live telemetry</span><h1>Pipeline topics</h1><p>Watch event throughput across every voice processing channel while audio moves through the system.</p></div><span className={`status ${topicError ? "warning" : "ok"}`}>{topicError ? "Telemetry stale" : "Live"}</span></header>
      <section className="upload-panel">
        <div><h2>Source audio</h2><p>Upload an audio file or capture the microphone, then stream it through the pipeline to Ogg/Opus output.</p></div>
        <div className="source-controls">
          <div className="source-tabs" role="group" aria-label="Audio source"><button className={source === "file" ? "selected" : "ghost"} onClick={() => setSource("file")}>Audio file</button><button className={source === "microphone" ? "selected" : "ghost"} onClick={() => setSource("microphone")}>Microphone</button></div>
          {source === "file" ? <div className="upload-controls"><label className="file-control"><span>{file ? file.name : "Choose an audio file"}</span><input aria-label="Audio file" type="file" accept="audio/*" onChange={(event) => setFile(event.target.files?.[0] ?? null)} /></label><button className="primary" disabled={!file || state === "Streaming" || state === "Connecting"} onClick={start}>Start stream</button></div> : <div className="microphone-control"><div><strong>{capture ? "Microphone live" : "Microphone ready"}</strong><span>{capture ? "Audio is streaming until you stop recording." : "Browser permission is requested when recording starts."}</span></div>{capture ? <button className="stop" onClick={() => capture.stop()}>Stop recording</button> : <button className="primary" disabled={state === "Requesting microphone"} onClick={start}>Start recording</button>}</div>}
        </div>
      </section>
      {error && <div className="notice error" role="alert">{error}</div>}
      <section className="activity">
        <div className="section-title"><div><h2>NATS subjects</h2><p>{topicError || (observedAt ? `Counts sampled ${new Date(observedAt).toLocaleTimeString()}` : "Connecting to pipeline telemetry")}</p></div>{session && <code>{session.slice(0, 16)}</code>}</div>
        <div className="topic-table" aria-label="NATS topic event activity" aria-live="polite">
          <div className="topic-row table-head"><span>Subject</span><span>Events / second</span><span>Total</span></div>
          {topics.length === 0 ? <div className="empty"><strong>No topic samples yet</strong><span>Waiting for the first NATS activity snapshot.</span></div> : topics.map((topic) => <TopicRow key={topic.subject} topic={topic} />)}
        </div>
      </section>
      {outputPaths.length > 0 && <section className="pipeline-result" aria-label="Pipeline output">{outputPaths.map((path, index) => <div key={path}><span>{index === 0 ? "English Ogg/Opus" : "Spanish Ogg/Opus"}</span><code>{path}</code></div>)}</section>}
    </main>
  </div>
}

function TopicRow({ topic }: { topic: TopicHistory }) {
  const peak = Math.max(1, ...topic.samples)
  return <div className="topic-row">
    <code className="topic-subject">{topic.subject}</code>
    <div className="histogram" aria-label={`${topic.subject} recent event counts`}>
      {topic.samples.map((count, index) => <i key={index} style={{ height: `${Math.max(2, count / peak * 100)}%` }} title={`${count} events`} />)}
    </div>
    <strong className="topic-total">{topic.total.toLocaleString()}</strong>
  </div>
}

import { useEffect, useState, type FormEvent } from "react"
import { currentUser, login, logout, uploadTicket, webTransportURL } from "./api"
import { streamFile } from "./transport"
import type { Event, User } from "./gen/qol/v1/qol_pb"

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
  const [state, setState] = useState("Ready")
  const [session, setSession] = useState("")
  const [events, setEvents] = useState<Event[]>([])
  const [error, setError] = useState("")

  async function start() {
    if (!file) return
    setState("Connecting")
    setError("")
    setEvents([])
    try {
      const ticket = await uploadTicket()
      await streamFile(webTransportURL(ticket), file, (frame) => {
        if (frame.body.case === "uploadAccepted") {
          setSession(frame.body.value.sessionId)
          setState("Streaming")
        }
        if (frame.body.case === "outputEvent") {
          const output = frame.body.value
          setEvents((current) => [...current, output])
        }
        if (frame.body.case === "uploadCompleted") setState(`Complete · ${frame.body.value.outputEvents} events`)
        if (frame.body.case === "error") throw new Error(frame.body.value.message)
      })
    } catch (problem) {
      setState("Failed")
      setError(problem instanceof Error ? problem.message : "Upload failed")
    }
  }

  return <div className="shell">
    <nav>
      <div className="wordmark">Qol</div>
      <div className="nav-item active"><span className="nav-icon">◉</span>Audio stream</div>
      <div className="account"><span>{user.username}</span><small>Administrator</small><button className="ghost" onClick={onLogout}>Sign out</button></div>
    </nav>
    <main className="workspace">
      <header><div><span className="eyebrow">Input · echo · output</span><h1>Audio stream</h1><p>Send an MP3 through the live PCM event pipeline and inspect each returned frame.</p></div><span className={`status ${state.startsWith("Complete") ? "ok" : "info"}`}>{state}</span></header>
      <section className="upload-panel">
        <div><h2>Source audio</h2><p>MP3 is decoded to 16 kHz mono signed PCM before entering the event bus.</p></div>
        <div className="upload-controls"><label className="file-control"><span>{file ? file.name : "Choose an MP3 file"}</span><input aria-label="MP3 file" type="file" accept="audio/mpeg,.mp3" onChange={(event) => setFile(event.target.files?.[0] ?? null)} /></label><button className="primary" disabled={!file || state === "Streaming" || state === "Connecting"} onClick={start}>Start stream</button></div>
      </section>
      {error && <div className="notice error" role="alert">{error}</div>}
      <section className="activity">
        <div className="section-title"><div><h2>Output events</h2><p>{events.length ? `${events.length} PCM frames observed` : "Events appear here as workers echo decoded audio."}</p></div>{session && <code>{session.slice(0, 16)}</code>}</div>
        <div className="event-table" role="table" aria-label="Output PCM events">
          <div className="event-row table-head" role="row"><span>Sequence</span><span>Span</span><span>Format</span><span>Frames</span><span>Bytes</span></div>
          {events.length === 0 ? <div className="empty"><strong>No output yet</strong><span>Select an MP3 and start the stream.</span></div> : events.map((event) => {
            const pcm = event.payload.case === "pcm" ? event.payload.value : undefined
            return <div className="event-row" role="row" key={event.eventId}><span>#{event.sequence.toString()}</span><span>{formatNs(event.span?.startNs ?? 0n)}–{formatNs(event.span?.endNs ?? 0n)}</span><span>16 kHz / mono</span><span>{pcm?.frameCount ?? 0}</span><span>{pcm?.interleavedSamples.length ?? 0}</span></div>
          })}
        </div>
      </section>
    </main>
  </div>
}

function formatNs(value: bigint): string {
  return `${(Number(value) / 1_000_000_000).toFixed(2)}s`
}

import { useCallback, useEffect, useState } from 'react'

// The operator's page.
//
// SEPARATE FROM THE SEAT MAP ON PURPOSE. Putting "fire 4000 buyers at the system"
// on the same screen as "buy a ticket" is a misclick waiting to happen. This is
// its own route and looks different so nobody arrives here by accident.
//
// THERE IS NO AUTH. Nothing in this system has any, which is consistent and fine
// on a LAN — but it means this page is a load weapon reachable by anyone who can
// reach the host, and that is the reason it should never be routed from outside.
export default function Admin() {
  const [events, setEvents] = useState([])
  const [eventID, setEventID] = useState('')
  const [buyers, setBuyers] = useState(2000)
  const [over, setOver] = useState(20)
  const [firing, setFiring] = useState(false)
  const [result, setResult] = useState(null)
  const [stats, setStats] = useState(null)
  const [bank, setBank] = useState({ decline_rate: 0.05, timeout_rate: 0.01 })
  const [msg, setMsg] = useState(null)

  useEffect(() => {
    ;(async () => {
      try {
        const r = await fetch('/api/events')
        const { events } = await r.json()
        setEvents(events ?? [])
        if (events?.length) setEventID(events[0].id)
      } catch (e) {
        setMsg({ kind: 'err', text: `could not list events: ${e.message}` })
      }
    })()
  }, [])

  const loadStats = useCallback(async () => {
    try {
      const r = await fetch('/admin/sim/stats')
      if (r.ok) setStats(await r.json())
    } catch {
      /* the simulator being unreachable is not worth a banner on every poll */
    }
  }, [])

  useEffect(() => {
    loadStats()
    const t = setInterval(loadStats, 3000)
    return () => clearInterval(t)
  }, [loadStats])

  async function fire() {
    if (!eventID || firing) return
    setFiring(true)
    setResult(null)
    setMsg({ kind: 'warn', text: 'on-sale running — this blocks until the last buyer is done' })
    try {
      const r = await fetch('/admin/sim/onsale', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          event_id: eventID,
          buyers: Number(buyers),
          over_seconds: Number(over),
        }),
      })
      if (!r.ok) throw new Error(`onsale → ${r.status}`)
      setResult(await r.json())
      setMsg(null)
    } catch (e) {
      setMsg({ kind: 'err', text: e.message })
    } finally {
      setFiring(false)
    }
  }

  async function setBankConfig(next) {
    setBank(next)
    try {
      const r = await fetch('/admin/bank/config', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(next),
      })
      if (!r.ok) throw new Error(`bank config → ${r.status}`)
      setMsg({ kind: 'ok', text: 'bank updated' })
    } catch (e) {
      setMsg({ kind: 'err', text: e.message })
    }
  }

  const ev = events.find((e) => e.id === eventID)

  return (
    <div className="wrap admin">
      <header>
        <h1>operator</h1>
        <p className="sub">
          load and chaos controls · <a href="/">back to the seat map</a>
        </p>
      </header>

      <div className="warn-banner">
        No authentication. Anything on this page affects the real system, and
        anyone who can reach this host can click it.
      </div>

      <section className="panel">
        <h2>On-sale</h2>
        <p className="hint">
          Every buyer is pinned to one event and goes for the best seats, which is
          what an on-sale actually is — thousands of people wanting the same seats
          in the same few seconds. It blocks until the last buyer finishes.
        </p>
        <div className="controls">
          <label>
            event
            <select value={eventID} onChange={(e) => setEventID(e.target.value)}>
              {events.map((e) => (
                <option key={e.id} value={e.id}>{e.title} — {e.venue}</option>
              ))}
            </select>
          </label>
          <label>
            buyers
            <input type="number" min="1" max="20000" value={buyers}
                   onChange={(e) => setBuyers(e.target.value)} />
          </label>
          <label>
            over (s)
            <input type="number" min="1" max="600" value={over}
                   onChange={(e) => setOver(e.target.value)} />
          </label>
          <button className="action danger" onClick={fire} disabled={firing || !eventID}>
            {firing ? 'running…' : `Fire ${buyers} buyers`}
          </button>
        </div>
        {ev && <p className="hint">target: {ev.title} at {ev.venue}</p>}

        {result && (
          <div className="counts">
            <div className="count"><b>{result.bought}</b><span>bought</span></div>
            <div className="count"><b>{result.lost_race_409}</b><span>lost the race</span></div>
            <div className="count"><b>{result.held}</b><span>held</span></div>
            <div className="count"><b>{result.errors}</b><span>errors</span></div>
            <div className="count"><b>{result.took}</b><span>took</span></div>
          </div>
        )}
      </section>

      <section className="panel">
        <h2>Simulator</h2>
        {stats ? (
          <div className="counts">
            <div className="count"><b>{stats.sessions}</b><span>sessions</span></div>
            <div className="count"><b>{stats.bought}</b><span>bought</span></div>
            <div className="count"><b>{stats.held}</b><span>held</span></div>
            <div className="count"><b>{stats.lost_409}</b><span>lost 409</span></div>
            <div className="count"><b>{stats.errors}</b><span>errors</span></div>
          </div>
        ) : (
          <p className="hint">simulator unreachable</p>
        )}
      </section>

      <section className="panel">
        <h2>The bank</h2>
        <p className="hint">
          It is supposed to misbehave — that is its whole job. Turning these up is
          how you find out what the saga does when money goes missing.
        </p>
        <div className="controls">
          <label>
            decline rate: {(bank.decline_rate * 100).toFixed(0)}%
            <input type="range" min="0" max="1" step="0.05" value={bank.decline_rate}
                   onChange={(e) => setBankConfig({ ...bank, decline_rate: Number(e.target.value) })} />
          </label>
          <label>
            timeout rate: {(bank.timeout_rate * 100).toFixed(0)}%
            <input type="range" min="0" max="1" step="0.05" value={bank.timeout_rate}
                   onChange={(e) => setBankConfig({ ...bank, timeout_rate: Number(e.target.value) })} />
          </label>
          <button className="action ghost"
                  onClick={() => setBankConfig({ decline_rate: 0.05, timeout_rate: 0.01 })}>
            Reset to tame
          </button>
        </div>
      </section>

      {msg && <div className={`msg ${msg.kind} standalone`}>{msg.text}</div>}
    </div>
  )
}

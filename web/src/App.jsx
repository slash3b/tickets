import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { customerID, api as fetchWithID, apiJSON } from './customer.js'
import SeatMap from './SeatMap.jsx'
import Upcoming from './Upcoming.jsx'

// The seat map is PUSHED now, over SSE. This is the fallback interval for when
// the stream is unavailable — no broker configured, or a proxy that will not hold
// the connection — and it is deliberately slower than the old 2s: polling is the
// degraded mode, not the design.
const FALLBACK_POLL_MS = 10000

// One id for the whole visit, so repeat purchases look like one person rather
// than a new customer each time. crypto.randomUUID needs a secure context; the
// Gateway also has a plain :80 listener, so fall back rather than throw there.
// The order's user_id stays a uuid because the database column is one. The
// customer id is the thing you filter telemetry by, and it is sent as a header on
// every request rather than only on the ones that buy something.
const USER_ID =
  globalThis.crypto?.randomUUID?.() ??
  '00000000-0000-4000-8000-' + Math.floor(Math.random() * 1e12).toString(16).padStart(12, '0')

export default function App() {
  const [events, setEvents] = useState([])
  const [event, setEvent] = useState(null)
  const [sections, setSections] = useState([])
  const [section, setSection] = useState(null)
  const [seats, setSeats] = useState([])
  const [picked, setPicked] = useState([])
  const [hold, setHold] = useState(null)
  const [expiresAt, setExpiresAt] = useState(null)
  const [left, setLeft] = useState(0)
  const [msg, setMsg] = useState(null)
  const busy = useRef(false)

  // What is on sale. Was events[0] — fine when the only thing that existed was
  // one cinema showing a day, wrong the moment an arena appeared alongside it.
  useEffect(() => {
    ;(async () => {
      try {
        const { events } = await apiJSON('/api/events')
        setEvents(events ?? [])
        if (events?.length) setEvent(events[0])
        else setMsg({ kind: 'err', text: 'Nothing on sale right now.' })
      } catch (e) {
        setMsg({ kind: 'err', text: `Could not reach the API: ${e.message}` })
      }
    })()
  }, [])

  // Sections follow the chosen event. A cinema has one; the arena has ten, and
  // picking between them is the difference between a seat map and a wall.
  useEffect(() => {
    if (!event) return
    setSection(null)
    setSections([])
    setSeats([])
    setPicked([])
    ;(async () => {
      try {
        const { sections } = await apiJSON(`/api/events/${event.id}/sections`)
        setSections(sections ?? [])
        if (sections?.length) setSection(sections[0])
      } catch (e) {
        setMsg({ kind: 'err', text: e.message })
      }
    })()
  }, [event])

  const refresh = useCallback(async () => {
    if (!event || !section) return
    try {
      const { seats } = await apiJSON(`/api/events/${event.id}/sections/${section.id}`)
      setSeats(seats ?? [])
    } catch {
      /* a failed poll is not worth shouting about; the next one will do */
    }
  }, [event, section])

  // ONE SECTION AT A TIME, NEVER THE WHOLE EVENT. The arena is 20,000 seats and
  // there is deliberately no endpoint that would return them all — at that size
  // it is a denial of service against your own database. This is why the section
  // picker exists rather than being a nicety.
  //
  // FETCH ONCE, THEN LISTEN. The initial load is the truth; the stream carries
  // changes to it. Polling every two seconds cost a gateway request, a catalog
  // call and an inventory call each time, almost always to be told nothing had
  // changed — a cost that grew with the size of the venue rather than with how
  // much was happening, which is exactly backwards during an on-sale.
  useEffect(() => { refresh() }, [refresh])

  useEffect(() => {
    if (!event) return
    const es = new EventSource(`/api/events/${event.id}/stream`)

    es.onmessage = (m) => {
      let change
      try { change = JSON.parse(m.data) } catch { return }
      if (!change?.seat_ids?.length || !change.status) return
      const touched = new Set(change.seat_ids)
      // Apply the change to whatever is on screen. Seats in other sections are
      // simply not present and fall away — the message carries every seat that
      // changed, and this section is only interested in its own.
      setSeats((prev) => {
        let hit = false
        const next = prev.map((s) => {
          if (!touched.has(s.id) || s.status === change.status) return s
          hit = true
          return { ...s, status: change.status }
        })
        return hit ? next : prev
      })
    }

    // A dropped stream must not leave a dead seat map on screen. EventSource
    // reconnects on its own; the refetch is what repairs anything missed while
    // it was gone, since the stream carries deltas and has no history.
    es.onerror = () => refresh()

    return () => es.close()
  }, [event, refresh])

  // Fallback polling, slow, for when there is no stream at all.
  useEffect(() => {
    const t = setInterval(refresh, FALLBACK_POLL_MS)
    return () => clearInterval(t)
  }, [refresh])

  useEffect(() => {
    if (!expiresAt) return setLeft(0)
    const tick = () => {
      const ms = expiresAt - Date.now()
      setLeft(Math.max(0, Math.ceil(ms / 1000)))
      if (ms <= 0) {
        setHold(null)
        setExpiresAt(null)
        setPicked([])
        setMsg({ kind: 'err', text: 'The hold expired and the seats went back.' })
      }
    }
    tick()
    const t = setInterval(tick, 1000)
    return () => clearInterval(t)
  }, [expiresAt])

  const counts = useMemo(() => {
    const c = { available: 0, held: 0, sold: 0 }
    for (const s of seats) if (c[s.status] !== undefined) c[s.status]++
    return c
  }, [seats])

  const pickedSet = useMemo(() => new Set(picked), [picked])

  const toggle = useCallback((seat) => {
    if (seat.status !== 'available') return
    setPicked((p) => (p.includes(seat.id) ? p.filter((x) => x !== seat.id) : [...p, seat.id]))
  }, [])

  async function holdSeats() {
    if (!picked.length || busy.current) return
    busy.current = true
    setMsg(null)
    try {
      const res = await fetchWithID('/api/holds', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ event_id: event.id, seat_ids: picked }),
      })
      if (res.status === 409) {
        setMsg({ kind: 'err', text: 'Someone just took one of those. Pick again.' })
        setPicked([])
        await refresh()
        return
      }
      if (!res.ok) throw new Error(`hold failed (${res.status})`)
      const body = await res.json()
      setHold(body.hold_id)
      setExpiresAt(new Date(body.expires_at).getTime())
      setMsg({ kind: 'ok', text: `Held ${picked.length} seat${picked.length > 1 ? 's' : ''}. Five minutes to decide.` })
      await refresh()
    } catch (e) {
      setMsg({ kind: 'err', text: e.message })
    } finally {
      busy.current = false
    }
  }

  async function buy() {
    if (!hold || busy.current) return
    busy.current = true
    setMsg(null)
    try {
      const body = await apiJSON('/api/orders', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          hold_id: hold,
          event_id: event.id,
          user_id: USER_ID,
          amount_minor: priceOf(section) * picked.length,
        }),
      })
      if (body.state === 'confirmed') {
        setMsg({ kind: 'ok', text: `Confirmed. ${picked.length} seat${picked.length > 1 ? 's are' : ' is'} yours.` })
      } else if (body.state === 'failed') {
        setMsg({ kind: 'err', text: 'The card was declined. The seats have gone back.' })
      } else {
        setMsg({ kind: 'ok', text: `Payment is still going through (${body.state}). It will settle shortly.` })
      }
      setHold(null)
      setExpiresAt(null)
      setPicked([])
      await refresh()
    } catch (e) {
      setMsg({ kind: 'err', text: e.message })
    } finally {
      busy.current = false
    }
  }

  async function release() {
    if (!hold) return
    await fetchWithID(`/api/holds/${hold}`, { method: 'DELETE' }).catch(() => {})
    setHold(null)
    setExpiresAt(null)
    setPicked([])
    setMsg(null)
    await refresh()
  }

  const total = (priceOf(section) * picked.length) / 100

  return (
    <div className="wrap">
      <header>
        <h1>{event?.title ?? 'tickets'}</h1>
        <p className="sub">
          {event ? `${event.venue} · ${new Date(event.starts_at).toLocaleString()}` : 'loading…'}
        </p>
        <div className="headrow">
          <button
            className="whoami"
            title="Your id in the traces and logs. Click to copy — paste it into SigNoz to see everything you did."
            onClick={() => navigator.clipboard?.writeText(customerID)}
          >
            you are <code>{customerID}</code>
          </button>
          <a className="oplink" href="/admin">operator →</a>
        </div>
      </header>

      {events.length > 1 && (
        <nav className="picker" aria-label="On sale now">
          {events.map((e) => (
            <button
              key={e.id}
              className={`chip ${event?.id === e.id ? 'on' : ''}`}
              onClick={() => setEvent(e)}
            >
              {e.title}
              <small>{e.venue}</small>
            </button>
          ))}
        </nav>
      )}

      <Upcoming />

      {sections.length > 1 && (
        <nav className="picker sections" aria-label="Sections">
          {sections.map((s) => (
            <button
              key={s.id}
              className={`chip ${section?.id === s.id ? 'on' : ''}`}
              onClick={() => { setSection(s); setPicked([]) }}
            >
              {s.name}
              <small>{s.seats} seats{s.price_minor ? ` · £${(s.price_minor / 100).toFixed(2)}` : ''}</small>
            </button>
          ))}
        </nav>
      )}

      <div className="counts">
        <div className="count"><b>{counts.available}</b><span>available</span></div>
        <div className="count"><b>{counts.held}</b><span>held</span></div>
        <div className="count"><b>{counts.sold}</b><span>sold</span></div>
        <div className="count"><b>{seats.length}</b><span>in {section?.name ?? 'section'}</span></div>
      </div>

      <div className="screen">{event?.venue?.match(/arena/i) ? 'stage' : 'screen'}</div>

      <SeatMap seats={seats} picked={pickedSet} onToggle={toggle} locked={!!hold} />

      <div className="legend">
        <span><i style={{ background: 'var(--available)' }} />available</span>
        <span><i style={{ background: 'var(--mine)' }} />your pick</span>
        <span><i style={{ background: 'var(--held)' }} />held by someone</span>
        <span><i style={{ background: 'var(--sold)' }} />sold</span>
      </div>

      <div className="bar">
        {!hold ? (
          <button className="action" onClick={holdSeats} disabled={!picked.length}>
            {picked.length ? `Hold ${picked.length} seat${picked.length > 1 ? 's' : ''}` : 'Pick a seat'}
          </button>
        ) : (
          <>
            <button className="action" onClick={buy} disabled={!priceOf(section)}>
              {priceOf(section) ? `Buy · £${total.toFixed(2)}` : 'No price set'}
            </button>
            <button className="action ghost" onClick={release}>Release</button>
            <span className="clock">{Math.floor(left / 60)}:{String(left % 60).padStart(2, '0')} left</span>
          </>
        )}
        {msg && <span className={`msg ${msg.kind}`}>{msg.text}</span>}
      </div>
    </div>
  )
}

// The price comes from catalog now, per section, which is what the arena needed:
// a floor seat and the back of Block 10 are not the same thing.
//
// It used to be hardcoded here to mirror the seeder — a guessed number that was
// then charged for real. Zero means catalog has no price for this section, and
// the UI says so rather than quietly offering something for nothing.
function priceOf(section) {
  return section?.price_minor ?? 0
}


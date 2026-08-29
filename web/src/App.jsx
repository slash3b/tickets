import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import SeatMap from './SeatMap.jsx'
import Upcoming from './Upcoming.jsx'

const POLL_MS = 2000

// One id for the whole visit, so repeat purchases look like one person rather
// than a new customer each time. crypto.randomUUID needs a secure context; the
// Gateway also has a plain :80 listener, so fall back rather than throw there.
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
        const { events } = await api('/api/events')
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
        const { sections } = await api(`/api/events/${event.id}/sections`)
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
      const { seats } = await api(`/api/events/${event.id}/sections/${section.id}`)
      setSeats(seats ?? [])
    } catch {
      /* a failed poll is not worth shouting about; the next one will do */
    }
  }, [event, section])

  // ONE SECTION AT A TIME, NEVER THE WHOLE EVENT. The arena is 20,000 seats and
  // there is deliberately no endpoint that would return them all — at that size
  // it is a denial of service against your own database. This is why the section
  // picker exists rather than being a nicety.
  useEffect(() => {
    refresh()
    const t = setInterval(refresh, POLL_MS)
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
      const res = await fetch('/api/holds', {
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
      const body = await api('/api/orders', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          hold_id: hold,
          event_id: event.id,
          user_id: USER_ID,
          amount_minor: price(event) * picked.length,
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
    await fetch(`/api/holds/${hold}`, { method: 'DELETE' }).catch(() => {})
    setHold(null)
    setExpiresAt(null)
    setPicked([])
    setMsg(null)
    await refresh()
  }

  const total = (price(event) * picked.length) / 100

  return (
    <div className="wrap">
      <header>
        <h1>{event?.title ?? 'tickets'}</h1>
        <p className="sub">
          {event ? `${event.venue} · ${new Date(event.starts_at).toLocaleString()}` : 'loading…'}
        </p>
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
              <small>{s.seats} seats</small>
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
            <button className="action" onClick={buy}>Buy · £{total.toFixed(2)}</button>
            <button className="action ghost" onClick={release}>Release</button>
            <span className="clock">{Math.floor(left / 60)}:{String(left % 60).padStart(2, '0')} left</span>
          </>
        )}
        {msg && <span className={`msg ${msg.kind}`}>{msg.text}</span>}
      </div>
    </div>
  )
}

// The API does not serve prices yet, so this mirrors what the seeder sets. It is
// a placeholder and says so: when catalog exposes event_prices this reads it
// instead, and until then a wrong number here would be charged for real.
function price(event) {
  return event?.venue?.match(/arena/i) ? 9500 : 1200
}

async function api(path, init) {
  const res = await fetch(path, init)
  if (!res.ok) throw new Error(`${path} → ${res.status}`)
  return res.json()
}

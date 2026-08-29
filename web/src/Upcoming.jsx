import { useEffect, useState } from 'react'

// Events that exist but cannot be bought yet.
//
// This is the visible half of milestone 8. A concert is announced long before
// the on-sale, and until that moment inventory has no seats for it at all — so
// there is nothing to draw a seat map from and nothing to hold. What there is,
// is a date.
export default function Upcoming() {
  const [events, setEvents] = useState([])
  const [now, setNow] = useState(Date.now())

  useEffect(() => {
    let alive = true
    const load = async () => {
      try {
        const res = await fetch('/api/events/upcoming')
        if (!res.ok) return
        const { events } = await res.json()
        if (alive) setEvents(events ?? [])
      } catch {
        /* nothing announced is the normal case; a failure here is not worth a banner */
      }
    }
    load()
    // Slower than the seat map: an on-sale date does not move, and the only
    // moment that matters is when an event LEAVES this list.
    const t = setInterval(load, 15000)
    return () => { alive = false; clearInterval(t) }
  }, [])

  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(t)
  }, [])

  if (!events.length) return null

  return (
    <section className="upcoming">
      <h2>On sale soon</h2>
      {events.map((e) => (
        <div className="soon" key={e.id}>
          <div>
            <b>{e.title}</b>
            <small>{e.venue} · {new Date(e.starts_at).toLocaleDateString()}</small>
          </div>
          <span className="countdown">{until(new Date(e.on_sale_at).getTime() - now)}</span>
        </div>
      ))}
    </section>
  )
}

function until(ms) {
  if (ms <= 0) return 'on sale now'
  const s = Math.floor(ms / 1000)
  const d = Math.floor(s / 86400)
  if (d >= 1) return `in ${d}d`
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (h >= 1) return `in ${h}h ${m}m`
  return `in ${m}m ${String(s % 60).padStart(2, '0')}s`
}

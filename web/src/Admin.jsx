import { useCallback, useEffect, useRef, useState } from 'react'

// The operator's page.
//
// SEPARATE FROM THE SEAT MAP ON PURPOSE. Putting "fire 4000 buyers at the system"
// on the same screen as "buy a ticket" is a misclick waiting to happen. This is
// its own route and looks different so nobody arrives here by accident.
//
// THERE IS NO AUTH. Nothing in this system has any, which is consistent and fine
// on a LAN — but it means this page is a load weapon reachable by anyone who can
// reach the host, and that is the reason it should never be routed from outside.
// What the bank runs at when nobody is experimenting: a realistic decline rate
// and the occasional lost answer. Named because it is both the initial state and
// what "Reset to tame" means.
const TAME = { decline_rate: 0.05, timeout_rate: 0.01 }

export default function Admin() {
  const [events, setEvents] = useState([])
  const [eventID, setEventID] = useState('')
  const [buyers, setBuyers] = useState(2000)
  const [over, setOver] = useState(20)
  const [firing, setFiring] = useState(false)
  const [result, setResult] = useState(null)
  const [stats, setStats] = useState(null)
  const [bank, setBank] = useState(TAME)
  // The slider's live value, read by the commit below. See the note there.
  const pending = useRef(TAME)
  const [msg, setMsg] = useState(null)
  const [staging, setStaging] = useState(false)
  const [staged, setStaged] = useState(null)
  const [onSaleIn, setOnSaleIn] = useState(60)
  const [venue, setVenue] = useState('arena')
  const [title, setTitle] = useState('')
  const [venueName, setVenueName] = useState('')
  const [sections, setSections] = useState(10)
  const [rows, setRows] = useState(50)
  const [perRow, setPerRow] = useState(40)

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

  // THE SLIDERS USED TO BE WRITE-ONLY. They rendered whatever this component's
  // initial state said, so after a reload the page claimed a tame 5% while the
  // bank might have been declining everything — which is exactly the situation
  // you open this page to find out about.
  useEffect(() => {
    ;(async () => {
      try {
        const r = await fetch('/admin/bank/config')
        if (r.ok) {
          const live = await r.json()
          pending.current = live
          setBank(live)
        }
      } catch {
        /* the bank being unreachable is its own visible problem elsewhere */
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

  // A DRAG IS ONE DECISION, NOT TWENTY. onChange fires per input event for a
  // range, so dragging decline_rate from 0 to 1 used to send about twenty PUTs,
  // each of them a real config change on a running system. The slider now moves
  // locally and commits once, when it is let go.
  //
  // THE COMMIT READS A REF, NOT THE RENDERED VALUE. Handing setBankConfig the
  // `bank` a handler closed over risks sending the value from the render before
  // the last drag event, which would leave the page showing one number and the
  // bank holding another — the exact bug the GET above exists to fix.
  function slide(next) {
    pending.current = next
    setBank(next)
  }
  const commit = () => setBankConfig(pending.current)

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

  // STAGE A SALE. Creates the showing and leaves it to go on sale on its own —
  // the workers loop opens the seats when the moment arrives. There is one path
  // that starts a sale and this does not become a second one.
  async function stage() {
    if (staging) return
    setStaging(true)
    setStaged(null)
    try {
      const body = {
        venue,
        on_sale_in_seconds: Number(onSaleIn),
        ...(title.trim() && { title: title.trim() }),
        ...(venue === 'custom' && {
          venue_name: venueName.trim(),
          sections: Number(sections),
          rows: Number(rows),
          seats_per_row: Number(perRow),
        }),
      }
      const r = await fetch('/api/admin/showings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      // The server explains a rejected layout in words; showing its reason beats
      // showing the operator a status code for something they can fix.
      if (!r.ok) {
        const { error } = await r.json().catch(() => ({}))
        throw new Error(error || `stage → ${r.status}`)
      }
      const s = await r.json()
      setStaged(s)
      setMsg({ kind: 'ok', text: `${s.title} created — ${s.seats} seats at ${s.venue}, on sale in ${onSaleIn}s` })

      // Pick it up as soon as it is buyable, so Fire targets it without anyone
      // having to hunt for the id.
      const poll = setInterval(async () => {
        try {
          const rr = await fetch('/api/events')
          const { events } = await rr.json()
          if (events?.some((e) => e.id === s.event_id)) {
            setEvents(events)
            setEventID(s.event_id)
            setMsg({ kind: 'ok', text: `${s.title} is ON SALE — ${s.seats} seats` })
            clearInterval(poll)
          }
        } catch { /* keep waiting */ }
      }, 3000)
      setTimeout(() => clearInterval(poll), 10 * 60 * 1000)
    } catch (e) {
      setMsg({ kind: 'err', text: e.message })
    } finally {
      setStaging(false)
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
        <h2>Stage a showing</h2>
        <p className="hint">
          Nothing creates showings on its own any more — the daily 03:00 CronJob is
          suspended, so this page is the only way one appears. Pick a room, and the
          seats open themselves when it goes on sale.
        </p>
        <div className="controls">
          <label>
            venue
            <select value={venue} onChange={(e) => setVenue(e.target.value)}>
              {/* No seat counts here either, for the reason on the button below:
                  the gateway owns the preset sizes. Arena and Cinema already say
                  which is the big one. */}
              <option value="arena">Arena</option>
              <option value="cinema">Cinema</option>
              <option value="custom">Custom…</option>
            </select>
          </label>
          <label>
            title
            <input type="text" placeholder="auto" value={title}
                   onChange={(e) => setTitle(e.target.value)} />
          </label>
          <label>
            on sale in (s)
            <input type="number" min="10" max="3600" value={onSaleIn}
                   onChange={(e) => setOnSaleIn(e.target.value)} />
          </label>
        </div>

        {venue === 'custom' && (
          <>
            <div className="controls">
              <label>
                venue name
                <input type="text" placeholder={`Custom ${sections}x${rows}x${perRow}`}
                       value={venueName} onChange={(e) => setVenueName(e.target.value)} />
              </label>
              <label>
                sections
                <input type="number" min="1" max="40" value={sections}
                       onChange={(e) => setSections(e.target.value)} />
              </label>
              <label>
                rows per section
                <input type="number" min="1" max="500" value={rows}
                       onChange={(e) => setRows(e.target.value)} />
              </label>
              <label>
                seats per row
                <input type="number" min="1" max="100" value={perRow}
                       onChange={(e) => setPerRow(e.target.value)} />
              </label>
            </div>
            <p className="hint">
              One section is priced and labelled as a screen; several become tiered
              blocks from the floor back. Leave the name blank and it is named after
              its shape, so two different rooms never end up sharing one seating
              chart. A name that already exists REUSES that chart, whatever the
              numbers here say.
            </p>
          </>
        )}

        {/* A STATIC LABEL, AND NO SEAT ARITHMETIC HERE.
            The button used to compute its own label — "Stage a 20,000-seat
            showing" — from a copy of the presets and the seat cap that the
            gateway already owns (arenaLayout, cinemaLayout and maxSeats in
            services/gateway/admin.go). Two copies of a number only stay equal
            until one of them changes, and this copy would have gone on stating
            the old size with complete confidence.
            The server validates the layout and explains a rejection in words;
            the line below reports the seats it actually built. */}
        <div className="controls">
          <button className="action" onClick={stage} disabled={staging}>
            {staging ? 'creating…' : 'Create'}
          </button>
        </div>

        {staged && (
          <p className="hint">
            {staged.title} · {staged.seats} seats at {staged.venue}
            {staged.venue_reused && ' (existing seating chart reused)'} · on sale at{' '}
            {new Date(staged.on_sale_at).toLocaleTimeString()} — the seats open
            themselves, then Fire below will target it.
          </p>
        )}
      </section>

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
                   onChange={(e) => slide({ ...bank, decline_rate: Number(e.target.value) })}
                   onPointerUp={commit}
                   onKeyUp={commit} />
          </label>
          <label>
            timeout rate: {(bank.timeout_rate * 100).toFixed(0)}%
            <input type="range" min="0" max="1" step="0.05" value={bank.timeout_rate}
                   onChange={(e) => slide({ ...bank, timeout_rate: Number(e.target.value) })}
                   onPointerUp={() => setBankConfig(bank)}
                   onKeyUp={() => setBankConfig(bank)} />
          </label>
          <button className="action ghost"
                  onClick={() => { pending.current = TAME; setBankConfig(TAME) }}>
            Reset to tame
          </button>
        </div>
      </section>

      {msg && <div className={`msg ${msg.kind} standalone`}>{msg.text}</div>}
    </div>
  )
}

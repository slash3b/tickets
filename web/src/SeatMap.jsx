import { memo, useMemo } from 'react'

// The seat map, sized for an arena block rather than a cinema screen.
//
// A CINEMA SECTION IS 96 SEATS AND AN ARENA BLOCK IS 2,000. The naive version —
// rebuild every seat button on every poll — was fine at 96 and is 2,000 DOM
// updates every two seconds at arena scale, on a page whose whole job is to feel
// live. Each seat is memoised on its own status, so a poll that changes four
// seats re-renders four buttons.
//
// It never has to handle 20,000, and that is not luck: there is no API that
// returns a whole event's seats, deliberately, so the most this can ever be
// asked to draw is one section.
function SeatMap({ seats, picked, onToggle, locked }) {
  const rows = useMemo(() => {
    const byRow = new Map()
    for (const s of seats) {
      if (!byRow.has(s.row)) byRow.set(s.row, [])
      byRow.get(s.row).push(s)
    }
    return [...byRow.entries()]
      // Row labels go A..Z then AA..AZ, so sort by length first — otherwise "AA"
      // sorts between "A" and "B" and the chart comes out shuffled.
      .sort(([a], [b]) => a.length - b.length || a.localeCompare(b))
      .map(([label, list]) => [label, list.sort((x, y) => x.number - y.number)])
  }, [seats])

  if (!seats.length) return <div className="rows empty">no seats to show</div>

  return (
    <div className={`rows ${seats.length > 400 ? 'dense' : ''}`}>
      {rows.map(([label, list]) => (
        <div className="row" key={label}>
          <span className="rowlabel">{label}</span>
          {list.map((s) => (
            <Seat
              key={s.id}
              seat={s}
              mine={picked.has(s.id)}
              locked={locked}
              onToggle={onToggle}
            />
          ))}
        </div>
      ))}
    </div>
  )
}

// memo with a comparator, because `seat` is a fresh object on every poll even
// when nothing about it changed. Without this the memo would never hit.
const Seat = memo(
  function Seat({ seat, mine, locked, onToggle }) {
    return (
      <button
        className={`seat ${seat.status} ${mine ? 'mine' : ''}`}
        onClick={() => onToggle(seat)}
        disabled={seat.status !== 'available' || locked}
        title={`${seat.row}${seat.number} · ${seat.status}`}
        aria-label={`Seat ${seat.row}${seat.number}, ${seat.status}`}
      />
    )
  },
  (a, b) =>
    a.seat.id === b.seat.id &&
    a.seat.status === b.seat.status &&
    a.mine === b.mine &&
    a.locked === b.locked,
)

export default memo(SeatMap)

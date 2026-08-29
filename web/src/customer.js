// Who you are, as far as the telemetry is concerned.
//
// A trace answers "what happened in this request". It cannot answer "show me
// everything I just did", which is the question you actually have when a seat
// disappears while you are looking at it. This id makes that one filter.
//
// IT SURVIVES A RELOAD, on purpose. The first version generated a fresh uuid per
// page load, so refreshing the page made you a different customer and split your
// session in half — which is exactly when you are most likely to be reloading.
//
// It is stored per browser and never leaves it except as a request header. There
// is no account behind it and it identifies nothing but a tab's owner to their
// own homelab.
const KEY = 'tickets.customer.id'

function generate() {
  // `ui-` so a human reading a trace can tell instantly whether a request came
  // from a browser or from the load generator, which prefixes its own with `sim-`.
  const rand =
    globalThis.crypto?.randomUUID?.().slice(0, 8) ??
    Math.floor(Math.random() * 1e10).toString(36)
  return `ui-${rand}`
}

export const customerID = (() => {
  try {
    const existing = localStorage.getItem(KEY)
    if (existing) return existing
    const fresh = generate()
    localStorage.setItem(KEY, fresh)
    return fresh
  } catch {
    // Private window, or storage blocked. A per-load id is worse than a stable
    // one and far better than none.
    return generate()
  }
})()

// api wraps fetch so the header cannot be forgotten on a new call. Every request
// the page makes carries the id, not only the ones that buy something.
export async function api(path, init = {}) {
  const res = await fetch(path, {
    ...init,
    headers: { ...(init.headers || {}), 'X-Customer-Id': customerID },
  })
  return res
}

export async function apiJSON(path, init) {
  const res = await api(path, init)
  if (!res.ok) throw new Error(`${path} → ${res.status}`)
  return res.json()
}

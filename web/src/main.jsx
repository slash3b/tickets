import React from 'react'
import { createRoot } from 'react-dom/client'
import App from './App.jsx'
import Admin from './Admin.jsx'
import './style.css'

// Two pages, no router library. One check of the path is the whole of the routing
// this app needs, and a dependency for that would be more code than it replaces.
//
// The admin page is deliberately not linked from the seat map: it is reachable by
// typing /admin, and nobody arrives there by clicking around while buying a ticket.
const page = window.location.pathname.startsWith('/admin') ? <Admin /> : <App />

createRoot(document.getElementById('root')).render(page)

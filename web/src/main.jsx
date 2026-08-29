import React from 'react'
import { createRoot } from 'react-dom/client'
import App from './App.jsx'
import Admin from './Admin.jsx'
import './style.css'

// Two pages, no router library. One check of the path is the whole of the routing
// this app needs, and a dependency for that would be more code than it replaces.
//
// The admin page is linked from the seat map header, styled to be the quietest
// thing there. It is an operator page on a LAN-only host, not a secret.
const page = window.location.pathname.startsWith('/admin') ? <Admin /> : <App />

createRoot(document.getElementById('root')).render(page)

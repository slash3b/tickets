import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  // In dev, proxy the API so the browser sees one origin — the same arrangement
  // nginx provides in production. Without it every call is a CORS problem that
  // only exists locally.
  server: {
    proxy: { '/api': 'http://localhost:8080' },
  },
})

import React, { useState } from 'react'
import { createRoot } from 'react-dom/client'
import { createBrowserRouter, RouterProvider, NavLink, Outlet, Link } from 'react-router-dom'
import './styles.css'
import { getTheme, toggleTheme } from './theme'
import Landing from './pages/Landing'
import Docs from './pages/Docs'
import Dashboard from './pages/Dashboard'
import Console from './pages/Console'

export function Mark({ size = 26 }: { size?: number }) {
  return (
    <svg className="mark" width={size} height={size} viewBox="0 0 24 24" fill="none" aria-hidden>
      <defs>
        <linearGradient id="vg" x1="0" y1="24" x2="24" y2="0">
          <stop offset="0" stopColor="#8b6dff" />
          <stop offset="1" stopColor="#34d6f0" />
        </linearGradient>
      </defs>
      <path d="M12 22V13" stroke="url(#vg)" strokeWidth="2.4" strokeLinecap="round" />
      <path d="M12 13C12 9.5 7 9.5 7 5.5" stroke="url(#vg)" strokeWidth="2.4" strokeLinecap="round" />
      <path d="M12 13C12 9.5 17 9.5 17 5.5" stroke="url(#vg)" strokeWidth="2.4" strokeLinecap="round" />
      <circle cx="12" cy="22" r="2.1" fill="url(#vg)" />
      <circle cx="7" cy="4.6" r="2.4" fill="url(#vg)" />
      <circle cx="17" cy="4.6" r="2.4" fill="url(#vg)" />
    </svg>
  )
}

function ThemeToggle() {
  const [theme, setTheme] = useState(getTheme())
  return (
    <button
      className="theme-toggle"
      title="Toggle theme"
      onClick={() => setTheme(toggleTheme())}
    >
      {theme === 'dark' ? '☀' : '☾'}
    </button>
  )
}

function Layout() {
  return (
    <>
      <header className="nav">
        <Link to="/" className="brand"><Mark /> Vectora<span>DB</span></Link>
        <div className="links">
          <NavLink to="/" end>Home</NavLink>
          <NavLink to="/docs">Docs</NavLink>
          <NavLink to="/dashboard">Dashboard</NavLink>
          <NavLink to="/console">Console</NavLink>
        </div>
        <div className="right">
          <a href="https://github.com/SauravYadav12/vectoraDB" target="_blank" rel="noreferrer" className="links"><span style={{ color: 'var(--muted)' }}>GitHub ↗</span></a>
          <ThemeToggle />
        </div>
      </header>
      <main className="container"><Outlet /></main>
      <footer className="container" style={{ paddingTop: 0, paddingBottom: 0 }}>
        <div className="footer">
          <Link to="/" className="brand"><Mark size={20} /> Vectora<span>DB</span></Link>
          <span className="muted">Serverless Postgres · branches · time-travel · agent DBs</span>
          <span style={{ marginLeft: 'auto' }} className="muted">Open source — AGPL-3.0 core · Apache-2.0 clients</span>
        </div>
      </footer>
    </>
  )
}

const router = createBrowserRouter([
  {
    path: '/',
    element: <Layout />,
    children: [
      { index: true, element: <Landing /> },
      { path: 'docs', element: <Docs /> },
      { path: 'dashboard', element: <Dashboard /> },
      { path: 'console', element: <Console /> },
    ],
  },
])

createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <RouterProvider router={router} />
  </React.StrictMode>,
)

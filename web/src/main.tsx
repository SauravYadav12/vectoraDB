import React, { useState, type ReactNode } from 'react'
import { createRoot } from 'react-dom/client'
import { createBrowserRouter, RouterProvider, NavLink, Outlet, Link, Navigate } from 'react-router-dom'
import './styles.css'
import { getTheme, toggleTheme } from './theme'
import { logout as apiLogout } from './api'
import { AuthProvider, useAuth } from './auth-context'
import Landing from './pages/Landing'
import Docs from './pages/Docs'
import Dashboard from './pages/Dashboard'
import Console from './pages/Console'
import Login from './pages/Login'
import ApiKeys from './pages/ApiKeys'

function RequireAuth({ children }: { children: ReactNode }) {
  const { user, loading } = useAuth()
  if (loading) return <div className="container muted">Loading…</div>
  if (!user) return <Navigate to="/login" replace />
  return <>{children}</>
}

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
    <button className="theme-toggle" title="Toggle theme" onClick={() => setTheme(toggleTheme())}>
      {theme === 'dark' ? '☀' : '☾'}
    </button>
  )
}

function UserMenu() {
  const { user, setUser } = useAuth()
  if (!user) return <NavLink to="/login" className="btn ghost" style={{ padding: '6px 12px' }}>Log in</NavLink>
  return (
    <div className="usermenu">
      <NavLink to="/keys" title="API keys" className="muted" style={{ fontSize: 13 }}>{user.email}</NavLink>
      <button className="ghost" onClick={async () => { await apiLogout().catch(() => {}); setUser(null); location.assign('/login') }}>Logout</button>
    </div>
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
          <NavLink to="/keys">API keys</NavLink>
        </div>
        <div className="right">
          <a href="https://github.com/SauravYadav12/vectoraDB" target="_blank" rel="noreferrer" className="muted" style={{ fontSize: 13 }}>GitHub ↗</a>
          <UserMenu />
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
      { path: 'login', element: <Login /> },
      { path: 'dashboard', element: <RequireAuth><Dashboard /></RequireAuth> },
      { path: 'console', element: <RequireAuth><Console /></RequireAuth> },
      { path: 'keys', element: <RequireAuth><ApiKeys /></RequireAuth> },
    ],
  },
])

createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <AuthProvider>
      <RouterProvider router={router} />
    </AuthProvider>
  </React.StrictMode>,
)

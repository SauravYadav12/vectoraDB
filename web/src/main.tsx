import React from 'react'
import { createRoot } from 'react-dom/client'
import { createBrowserRouter, RouterProvider, NavLink, Outlet } from 'react-router-dom'
import './styles.css'
import Landing from './pages/Landing'
import Docs from './pages/Docs'
import Dashboard from './pages/Dashboard'
import Console from './pages/Console'

function Layout() {
  return (
    <>
      <header className="nav">
        <NavLink to="/" className="brand">Vectora<span>DB</span></NavLink>
        <nav>
          <NavLink to="/" end>Home</NavLink>
          <NavLink to="/docs">Docs</NavLink>
          <NavLink to="/dashboard">Dashboard</NavLink>
          <NavLink to="/console">Console</NavLink>
          <a href="https://github.com/SauravYadav12/vectoraDB" target="_blank" rel="noreferrer">GitHub</a>
        </nav>
      </header>
      <main className="container"><Outlet /></main>
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

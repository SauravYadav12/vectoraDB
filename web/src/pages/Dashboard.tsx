import { useEffect, useState } from 'react'
import {
  getStatus, getBranches, createBranch, deleteBranch, suspendBranch, resumeBranch,
  API, type Status, type Branch,
} from '../api'

function Dot({ up }: { up: boolean }) {
  return <span className={'dot ' + (up ? 'up' : 'down')} />
}

export default function Dashboard() {
  const [status, setStatus] = useState<Status | null>(null)
  const [branches, setBranches] = useState<Branch[]>([])
  const [name, setName] = useState('')
  const [err, setErr] = useState('')
  const [offline, setOffline] = useState(false)

  const refresh = async () => {
    try {
      const [s, b] = await Promise.all([getStatus(), getBranches()])
      setStatus(s); setBranches(b); setOffline(false)
    } catch {
      setOffline(true)
    }
  }
  useEffect(() => {
    refresh()
    const t = setInterval(refresh, 3000)
    return () => clearInterval(t)
  }, [])

  const act = async (fn: () => Promise<unknown>) => {
    setErr('')
    try { await fn(); await refresh() } catch (e) { setErr((e as Error).message) }
  }
  const create = () => {
    const n = name.trim()
    if (n) { act(() => createBranch(n)); setName('') }
  }

  if (offline) {
    return (
      <>
        <h1>Dashboard</h1>
        <div className="offline">
          Can't reach the API at <code>{API}</code>. Start it with{' '}
          <code>lima /tmp/vectoradb start</code>, or set <code>VITE_API_URL</code>.
        </div>
      </>
    )
  }

  const cards: [string, JSX.Element | string | number][] = status ? [
    ['Primary', <span><Dot up={status.mainReady} /> {status.mainReady ? 'Ready' : 'Down'}</span>],
    ['Branches', status.branches],
    ['Agent DBs', status.agents],
    ['Proxy', <span><Dot up={status.servers.proxy} /> {status.servers.proxy ? 'Up' : 'Down'}</span>],
    ['Replica', status.ha.enabled
      ? <span><Dot up={status.ha.streaming} /> {status.ha.streaming ? 'streaming' : status.ha.standby}</span>
      : <span className="muted">none</span>],
    ['Storage', status.storage?.used || '—'],
  ] : []

  return (
    <>
      <h1>Dashboard</h1>
      <div className="cards">
        {cards.map(([k, v], i) => (
          <div className="card" key={i}><div className="k">{k}</div><div className="v">{v}</div></div>
        ))}
      </div>

      <div className="row">
        <input
          placeholder="new branch name (e.g. qa)"
          value={name}
          onChange={e => setName(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter') create() }}
        />
        <button onClick={create}>Create branch</button>
      </div>
      {err && <div className="err">{err}</div>}

      <table>
        <thead>
          <tr><th>Branch</th><th>Type</th><th>State</th><th>Size (CoW)</th><th>Conns</th><th>Connection string</th><th /></tr>
        </thead>
        <tbody>
          {branches
            .slice()
            .sort((a, b) => (a.primary ? -1 : b.primary ? 1 : a.name.localeCompare(b.name)))
            .map(b => {
              const running = b.state === 'running'
              const dsn = `postgres://vectoradb:vectoradb@localhost:6432/${b.name}`
              const type = b.primary ? 'primary' : b.agent ? 'agent' : 'branch'
              return (
                <tr key={b.name}>
                  <td><b>{b.name}</b></td>
                  <td><span className={'badge ' + type}>{type}</span></td>
                  <td><span className={'state ' + (running ? 'running' : 'suspended')}><Dot up={running} /> {running ? 'running' : 'suspended'}</span></td>
                  <td>{b.used || '—'}</td>
                  <td>{running ? b.connections : '—'}</td>
                  <td><span className="mono" title="click to copy" onClick={() => navigator.clipboard?.writeText(dsn)}>{dsn}</span></td>
                  <td>
                    <div className="actions">
                      {!b.primary && (running
                        ? <button className="ghost" onClick={() => act(() => suspendBranch(b.name))}>Suspend</button>
                        : <button className="ghost" onClick={() => act(() => resumeBranch(b.name))}>Resume</button>)}
                      {!b.primary && (
                        <button className="ghost danger" onClick={() => { if (confirm(`Delete branch "${b.name}"? This destroys its data.`)) act(() => deleteBranch(b.name)) }}>Delete</button>
                      )}
                    </div>
                  </td>
                </tr>
              )
            })}
        </tbody>
      </table>
    </>
  )
}

import { useEffect, useState } from 'react'
import {
  getStatus, getBranches, createBranch, deleteBranch, suspendBranch, resumeBranch,
  API, type Status, type Branch,
} from '../api'
import { useConfirm } from '../confirm'

function Dot({ up }: { up: boolean }) {
  return <span className={'dot ' + (up ? 'up' : 'down')} />
}

function toBytes(s: string): number {
  const m = /^([\d.]+)\s*([KMGT]?)/i.exec(s || '')
  if (!m) return 0
  const mult: Record<string, number> = { '': 1, K: 1024, M: 1024 ** 2, G: 1024 ** 3, T: 1024 ** 4 }
  return parseFloat(m[1]) * (mult[m[2].toUpperCase()] || 1)
}

export default function Dashboard() {
  const confirm = useConfirm()
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
          Can’t reach the API at <code>{API}</code>. Start it with{' '}
          <code>vdb start</code>, or set <code>VITE_API_URL</code>.
        </div>
      </>
    )
  }

  const tiles: [string, React.ReactNode][] = status ? [
    ['Primary', <><Dot up={status.mainReady} /> {status.mainReady ? 'Ready' : 'Down'}</>],
    ['Branches', status.branches],
    ['Agent DBs', status.agents],
    ['Gateway', <><Dot up={status.servers.gateway} /> {status.servers.gateway ? 'Up' : 'Down'}</>],
    ['Replica', status.ha.enabled
      ? <><Dot up={status.ha.streaming} /> {status.ha.streaming ? 'streaming' : status.ha.standby}</>
      : <span style={{ color: 'var(--muted)', fontSize: 18 }}>none</span>],
    ['Storage', status.storage?.used || '—'],
  ] : []

  const maxUsed = Math.max(1, ...branches.map(b => toBytes(b.used)))

  return (
    <div className="fade-up">
      <h1>Dashboard</h1>
      <p className="muted" style={{ marginTop: -2 }}>Live control plane · auto-refreshing every 3s.</p>

      <div className="grid stat-grid">
        {tiles.map(([k, v], i) => (
          <div className="tile" key={i}><div className="k">{k}</div><div className="v">{v}</div></div>
        ))}
      </div>

      <div className="row">
        <input
          placeholder="new branch name (e.g. qa)"
          value={name}
          onChange={e => setName(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter') create() }}
          style={{ minWidth: 240 }}
        />
        <button className="primary" onClick={create} disabled={!name.trim()}>+ Create branch</button>
      </div>
      {err && <div className="err">{err}</div>}

      <div className="table-wrap">
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
                const pct = Math.max(6, Math.round((toBytes(b.used) / maxUsed) * 100))
                return (
                  <tr key={b.name}>
                    <td><b>{b.name}</b></td>
                    <td><span className={'badge ' + type}>{type}</span></td>
                    <td><span className={'state ' + (running ? 'running' : 'suspended')}><Dot up={running} /> {running ? 'running' : 'suspended'}</span></td>
                    <td>
                      <div style={{ display: 'flex', alignItems: 'center', gap: 9 }}>
                        <div className="cow"><span style={{ width: pct + '%' }} /></div>
                        <span className="mono muted" style={{ fontSize: 12 }}>{b.used || '—'}</span>
                      </div>
                    </td>
                    <td>{running ? b.connections : '—'}</td>
                    <td><span className="dsn" title="click to copy" onClick={() => navigator.clipboard?.writeText(dsn)}>{dsn}</span></td>
                    <td>
                      <div className="actions">
                        {!b.primary && (running
                          ? <button className="ghost" onClick={() => act(() => suspendBranch(b.name))}>Suspend</button>
                          : <button className="ghost" onClick={() => act(() => resumeBranch(b.name))}>Resume</button>)}
                        {!b.primary && (
                          <button className="ghost danger" onClick={async () => {
                            const ok = await confirm({
                              title: 'Delete branch',
                              message: <>Delete branch <b>{b.name}</b>? This permanently destroys its data and can't be undone.</>,
                              confirmText: 'Delete', danger: true,
                            })
                            if (ok) act(() => deleteBranch(b.name))
                          }}>Delete</button>
                        )}
                      </div>
                    </td>
                  </tr>
                )
              })}
          </tbody>
        </table>
      </div>
    </div>
  )
}

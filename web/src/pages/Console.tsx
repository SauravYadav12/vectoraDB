import { useEffect, useState } from 'react'
import { getBranches, runQuery, API, type Branch, type QueryResult } from '../api'

export default function Console() {
  const [branches, setBranches] = useState<Branch[]>([])
  const [name, setName] = useState('main')
  const [sql, setSql] = useState('SELECT version();')
  const [res, setRes] = useState<QueryResult | null>(null)
  const [busy, setBusy] = useState(false)
  const [offline, setOffline] = useState(false)

  useEffect(() => {
    getBranches()
      .then(b => { setBranches(b); setOffline(false) })
      .catch(() => setOffline(true))
  }, [])

  const run = async () => {
    setBusy(true); setRes(null)
    try { setRes(await runQuery(name, sql)) } catch (e) { setRes({ error: (e as Error).message }) } finally { setBusy(false) }
  }

  if (offline) {
    return (
      <>
        <h1>SQL Console</h1>
        <div className="offline">Can’t reach the API at <code>{API}</code>. Start it with <code>lima /tmp/vectoradb start</code>.</div>
      </>
    )
  }

  const options = branches.length ? branches : ([{ name: 'main' }] as Branch[])

  return (
    <div className="fade-up">
      <h1>SQL Console</h1>
      <p className="muted" style={{ marginTop: -2 }}>Run SQL against any branch, through the control-plane API.</p>

      <div className="row">
        <span className="muted" style={{ fontSize: 13 }}>Branch</span>
        <select value={name} onChange={e => setName(e.target.value)}>
          {options.map(b => <option key={b.name} value={b.name}>{b.name}</option>)}
        </select>
        <button className="primary" onClick={run} disabled={busy}>{busy ? 'Running…' : 'Run  ⌘/Ctrl+↵'}</button>
      </div>

      <textarea
        className="editor"
        value={sql}
        spellCheck={false}
        onChange={e => setSql(e.target.value)}
        onKeyDown={e => { if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') run() }}
      />

      {res && <Results res={res} />}
    </div>
  )
}

function Results({ res }: { res: QueryResult }) {
  if (res.error) return <div className="err">{res.error}</div>
  const cols = res.columns || []
  const rows = res.rows || []
  return (
    <>
      <div className="okmsg">✓ {res.command || 'ok'} · {rows.length} row{rows.length === 1 ? '' : 's'}</div>
      {cols.length > 0 && (
        <div className="grid-wrap table-wrap">
          <table>
            <thead><tr>{cols.map((c, i) => <th key={i}>{c}</th>)}</tr></thead>
            <tbody>
              {rows.map((r, i) => (
                <tr key={i}>{r.map((v, j) => <td key={j}>{v === null ? 'NULL' : String(v)}</td>)}</tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}

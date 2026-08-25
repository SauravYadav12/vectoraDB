import { useCallback, useEffect, useState } from 'react'
import { getBranches, getLedger, type Branch, type QueryResult } from '../api'

type Row = Record<string, string | null>

const STATUS = ['', 'APPLIED', 'FLAGGED', 'BLOCKED'] as const
const KIND = ['', 'human', 'agent'] as const

// Turn the {columns, rows} query result into keyed objects.
function toRows(res: QueryResult | null): Row[] {
  if (!res || !res.columns || !res.rows) return []
  return res.rows.map(r => {
    const o: Row = {}
    res.columns!.forEach((c, i) => { o[c] = r[i] as string | null })
    return o
  })
}

export default function Ledger() {
  const [branches, setBranches] = useState<Branch[]>([])
  const [branch, setBranch] = useState('main')
  const [status, setStatus] = useState('')
  const [kind, setKind] = useState('')
  const [actor, setActor] = useState('')
  const [res, setRes] = useState<QueryResult | null>(null)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => { getBranches().then(setBranches).catch(() => {}) }, [])

  const load = useCallback(async () => {
    setBusy(true); setErr('')
    try { setRes(await getLedger(branch, { status, kind, actor })) }
    catch (e) { setErr((e as Error).message) }
    finally { setBusy(false) }
  }, [branch, status, kind, actor])

  useEffect(() => { load() }, [load])

  const rows = toRows(res)
  const options = branches.length ? branches : ([{ name: 'main' }] as Branch[])

  return (
    <div className="fade-up">
      <h1>Schema Ledger</h1>
      <p className="lead" style={{ marginTop: -2 }}>
        A record the database keeps about itself — every schema change, attributed and policy-checked.
      </p>

      <div className="row" style={{ flexWrap: 'wrap', gap: 10 }}>
        <span className="muted" style={{ fontSize: 13 }}>Branch</span>
        <select value={branch} onChange={e => setBranch(e.target.value)}>
          {options.map(b => <option key={b.name} value={b.name}>{b.name}</option>)}
        </select>
        <div className="seg">
          {STATUS.map(s => (
            <button key={s || 'all'} className={status === s ? 'active' : ''} onClick={() => setStatus(s)}>
              {s || 'All'}
            </button>
          ))}
        </div>
        <div className="seg">
          {KIND.map(k => (
            <button key={k || 'all'} className={kind === k ? 'active' : ''} onClick={() => setKind(k)}>
              {k ? k[0].toUpperCase() + k.slice(1) : 'Any actor'}
            </button>
          ))}
        </div>
        <input placeholder="filter by actor…" value={actor} onChange={e => setActor(e.target.value)} style={{ minWidth: 160 }} />
        <button className="ghost" onClick={load} disabled={busy}>{busy ? '…' : 'Refresh'}</button>
      </div>

      {err && <div className="err">{err.includes('schema_ledger') ? 'The ledger is not installed on this branch yet.' : err}</div>}

      <div className="table-wrap" style={{ marginTop: 14 }}>
        <table>
          <thead>
            <tr><th>Time</th><th>Actor</th><th>Tool</th><th>Change</th><th>Status</th></tr>
          </thead>
          <tbody>
            {rows.length === 0
              ? <tr><td colSpan={5} className="muted">no changes recorded yet</td></tr>
              : rows.map((r, i) => (
                <tr key={i}>
                  <td className="mono muted" style={{ whiteSpace: 'nowrap' }}>{r.at}</td>
                  <td style={{ whiteSpace: 'nowrap' }}>
                    <span className={'lg-kind ' + (r.actor_kind || 'human')}>{r.actor_kind || 'human'}</span>{' '}
                    <span>{r.actor || '—'}</span>
                  </td>
                  <td className="muted" style={{ whiteSpace: 'nowrap' }}>{r.tool || '—'}</td>
                  <td>
                    <code className="mono">{r.command_tag}</code>
                    {r.object_identity && <span className="muted"> {r.object_identity}</span>}
                    {r.risk && <span className="lg-risk"> · {r.risk}</span>}
                  </td>
                  <td><span className={'lg-status ' + (r.status || '')}>{r.status}</span></td>
                </tr>
              ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

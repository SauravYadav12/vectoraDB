import { useCallback, useEffect, useState } from 'react'
import { getBranches, runQuery, API, type Branch, type QueryResult } from '../api'

type DbObject = { schema: string; name: string; type: 'table' | 'view' }
type Tab = 'rows' | 'structure' | 'indexes'

const PAGE = 100

const lit = (s: string) => "'" + s.replace(/'/g, "''") + "'"
const qid = (s: string) => '"' + s.replace(/"/g, '""') + '"'
const rel = (o: DbObject) => qid(o.schema) + '.' + qid(o.name)
const key = (o: DbObject) => o.schema + '.' + o.name

const LIST_SQL = `SELECT table_schema, table_name, table_type
FROM information_schema.tables
WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
ORDER BY table_schema, table_type DESC, table_name`

export default function Console() {
  const [branches, setBranches] = useState<Branch[]>([])
  const [branch, setBranch] = useState('main')
  const [offline, setOffline] = useState(false)
  const [objects, setObjects] = useState<DbObject[]>([])
  const [filter, setFilter] = useState('')

  const [mode, setMode] = useState<'query' | 'browse'>('query')
  const [sel, setSel] = useState<DbObject | null>(null)
  const [tab, setTab] = useState<Tab>('rows')
  const [page, setPage] = useState(0)
  const [total, setTotal] = useState<number | null>(null)

  const [browseRes, setBrowseRes] = useState<QueryResult | null>(null)
  const [sql, setSql] = useState('SELECT version();')
  const [queryRes, setQueryRes] = useState<QueryResult | null>(null)
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    getBranches().then(b => { setBranches(b); setOffline(false) }).catch(() => setOffline(true))
  }, [])

  const loadObjects = useCallback(async (b: string) => {
    try {
      const r = await runQuery(b, LIST_SQL)
      setObjects((r.rows || []).map(row => ({
        schema: String(row[0]), name: String(row[1]),
        type: /view/i.test(String(row[2])) ? 'view' : 'table',
      })))
    } catch { setObjects([]) }
  }, [])

  // Reset when the branch changes.
  useEffect(() => { loadObjects(branch); setSel(null); setMode('query'); setBrowseRes(null) }, [branch, loadObjects])

  // Load the active browse tab.
  useEffect(() => {
    if (mode !== 'browse' || !sel) return
    let cancelled = false
    const load = async () => {
      setBusy(true); setBrowseRes(null)
      try {
        let q = ''
        if (tab === 'rows') {
          q = `SELECT * FROM ${rel(sel)} LIMIT ${PAGE} OFFSET ${page * PAGE}`
        } else if (tab === 'structure') {
          q = `SELECT column_name, data_type, is_nullable, column_default
               FROM information_schema.columns
               WHERE table_schema=${lit(sel.schema)} AND table_name=${lit(sel.name)}
               ORDER BY ordinal_position`
        } else {
          q = `SELECT indexname, indexdef FROM pg_indexes
               WHERE schemaname=${lit(sel.schema)} AND tablename=${lit(sel.name)}
               ORDER BY indexname`
        }
        const r = await runQuery(branch, q)
        if (!cancelled) setBrowseRes(r)
        if (tab === 'rows' && !cancelled) {
          const c = await runQuery(branch, `SELECT count(*) FROM ${rel(sel)}`)
          if (!cancelled) setTotal(Number(c.rows?.[0]?.[0] ?? 0))
        }
      } catch (e) {
        if (!cancelled) setBrowseRes({ error: (e as Error).message })
      } finally {
        if (!cancelled) setBusy(false)
      }
    }
    load()
    return () => { cancelled = true }
  }, [mode, sel, tab, page, branch])

  const openObject = (o: DbObject) => { setSel(o); setTab('rows'); setPage(0); setTotal(null); setMode('browse') }
  const switchTab = (t: Tab) => { setTab(t); if (t === 'rows') setPage(0) }

  const runSql = async () => {
    setBusy(true); setQueryRes(null); setMode('query')
    try { setQueryRes(await runQuery(branch, sql)) } catch (e) { setQueryRes({ error: (e as Error).message }) } finally { setBusy(false) }
  }

  if (offline) {
    return (
      <>
        <h1>SQL Console</h1>
        <div className="offline">Can’t reach the API at <code>{API}</code>. Start it with <code>vdb start</code>.</div>
      </>
    )
  }

  const options = branches.length ? branches : ([{ name: 'main' }] as Branch[])
  const f = filter.trim().toLowerCase()
  const match = (o: DbObject) => !f || o.name.toLowerCase().includes(f) || o.schema.toLowerCase().includes(f)
  const tables = objects.filter(o => o.type === 'table' && match(o))
  const views = objects.filter(o => o.type === 'view' && match(o))
  const label = (o: DbObject) => (o.schema === 'public' ? o.name : o.schema + '.' + o.name)

  return (
    <div className="fade-up">
      <h1>SQL Console</h1>
      <p className="muted" style={{ marginTop: -2 }}>Browse a branch’s tables &amp; views, or run any SQL — through the control-plane API.</p>

      <div className="row">
        <span className="muted" style={{ fontSize: 13 }}>Branch</span>
        <select value={branch} onChange={e => setBranch(e.target.value)}>
          {options.map(b => <option key={b.name} value={b.name}>{b.name}</option>)}
        </select>
      </div>

      <div className="console-grid">
        <aside className="obj-panel">
          <div className="obj-head">
            <span>Schema</span>
            <button title="Refresh" onClick={() => loadObjects(branch)}>↻</button>
          </div>
          <div style={{ padding: 8 }}>
            <input className="obj-filter" placeholder="Filter tables…" value={filter} onChange={e => setFilter(e.target.value)} />
          </div>
          <div className="obj-list">
            <button className={'obj-item special' + (mode === 'query' ? ' active' : '')} onClick={() => setMode('query')}>
              <i className="ic">⌘</i>SQL query
            </button>
            {objects.length === 0 && (
              <div className="obj-empty">No tables yet. Create one with <code>CREATE TABLE …</code> in the SQL query.</div>
            )}
            {tables.length > 0 && <div className="obj-group">Tables</div>}
            {tables.map(o => (
              <button key={key(o)} className={'obj-item' + (mode === 'browse' && sel && key(sel) === key(o) ? ' active' : '')} onClick={() => openObject(o)}>
                <i className="ic">▦</i>{label(o)}
              </button>
            ))}
            {views.length > 0 && <div className="obj-group">Views</div>}
            {views.map(o => (
              <button key={key(o)} className={'obj-item' + (mode === 'browse' && sel && key(sel) === key(o) ? ' active' : '')} onClick={() => openObject(o)}>
                <i className="ic">◈</i>{label(o)}
              </button>
            ))}
          </div>
        </aside>

        <div>
          {mode === 'query' ? (
            <>
              <textarea
                className="editor"
                value={sql}
                spellCheck={false}
                onChange={e => setSql(e.target.value)}
                onKeyDown={e => { if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') runSql() }}
              />
              <div className="row" style={{ marginTop: 10 }}>
                <button className="primary" onClick={runSql} disabled={busy}>{busy ? 'Running…' : 'Run  ⌘/Ctrl+↵'}</button>
              </div>
              {queryRes && <Grid res={queryRes} showCommand />}
            </>
          ) : sel && (
            <>
              <div className="obj-view-head">
                <h2>{sel.name}</h2>
                <span className="path">{sel.schema} · {sel.type}</span>
              </div>
              <div className="db-tabs">
                {(['rows', 'structure', 'indexes'] as Tab[]).map(t => (
                  <button key={t} className={tab === t ? 'active' : ''} onClick={() => switchTab(t)}>
                    {t === 'rows' ? 'Rows' : t === 'structure' ? 'Structure' : 'Indexes'}
                  </button>
                ))}
              </div>
              {busy && !browseRes ? <div className="muted">Loading…</div> : browseRes && <Grid res={browseRes} />}
              {tab === 'rows' && browseRes && !browseRes.error && (
                <div className="pager">
                  <button className="ghost" onClick={() => setPage(p => Math.max(0, p - 1))} disabled={page === 0}>‹ Prev</button>
                  <span>
                    rows {total === 0 ? 0 : page * PAGE + 1}–{page * PAGE + (browseRes.rows?.length || 0)}
                    {total != null && <> of {total}</>}
                  </span>
                  <button className="ghost" onClick={() => setPage(p => p + 1)}
                    disabled={total == null ? (browseRes.rows?.length || 0) < PAGE : (page + 1) * PAGE >= total}>Next ›</button>
                </div>
              )}
            </>
          )}
        </div>
      </div>
    </div>
  )
}

function Grid({ res, showCommand }: { res: QueryResult; showCommand?: boolean }) {
  if (res.error) return <div className="err">{res.error}</div>
  const cols = res.columns || []
  const rows = res.rows || []
  return (
    <>
      {showCommand && <div className="okmsg">✓ {res.command || 'ok'} · {rows.length} row{rows.length === 1 ? '' : 's'}</div>}
      {cols.length > 0 ? (
        <div className="grid-wrap table-wrap">
          <table>
            <thead><tr>{cols.map((c, i) => <th key={i}>{c}</th>)}</tr></thead>
            <tbody>
              {rows.length === 0
                ? <tr><td colSpan={cols.length} className="muted">no rows</td></tr>
                : rows.map((r, i) => (
                  <tr key={i}>{r.map((v, j) => <td key={j}>{v === null ? 'NULL' : String(v)}</td>)}</tr>
                ))}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="okmsg">✓ {res.command || 'ok'}</div>
      )}
    </>
  )
}

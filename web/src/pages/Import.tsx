import { useState } from 'react'
import { Link } from 'react-router-dom'
import { importDB, importFile } from '../api'

export default function Import() {
  const [source, setSource] = useState('')
  const [continuous, setContinuous] = useState(false)
  const [file, setFile] = useState<File | null>(null)
  const [target, setTarget] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [done, setDone] = useState<{ target: string; tables: number; status?: string } | null>(null)

  const finish = (r: { target: string; tables: number; status?: string }) => { setDone(r); setBusy(false) }
  const fail = (e: unknown) => { setErr((e as Error).message); setBusy(false) }

  const isPg = /^postgres(ql)?:\/\//.test(source.trim())

  const runDSN = async () => {
    if (!source.trim()) return
    setBusy(true); setErr(''); setDone(null)
    try { finish(await importDB(source.trim(), target.trim(), continuous && isPg)) } catch (e) { fail(e) }
  }
  const runFile = async () => {
    if (!file) return
    setBusy(true); setErr(''); setDone(null)
    try { finish(await importFile(file, target.trim())) } catch (e) { fail(e) }
  }

  return (
    <div className="fade-up" style={{ maxWidth: 720 }}>
      <h1>Migrate a database</h1>
      <p className="lead" style={{ marginTop: -2 }}>
        Move an existing database into a fresh VectoraDB instance. Because VectoraDB <em>is</em> PostgreSQL,
        a Postgres source migrates with full fidelity.
      </p>

      <label className="fld" style={{ marginBottom: 14 }}>
        <span>New instance name <span className="muted">(optional)</span></span>
        <input placeholder="auto — named from the source" value={target} onChange={e => setTarget(e.target.value)} style={{ maxWidth: 320 }} />
      </label>

      <div className="panel" style={{ padding: 18 }}>
        <h3 style={{ margin: '0 0 10px' }}>From a live database</h3>
        <label className="fld">
          <span>Source connection string</span>
          <input
            placeholder="postgres:// · mysql:// · mariadb:// · mongodb://"
            value={source}
            onChange={e => setSource(e.target.value)}
            spellCheck={false}
            style={{ fontFamily: 'var(--font-mono)', fontSize: 13 }}
          />
          <span className="hint">
            Postgres/Postgres-wire (RDS, Neon, Supabase, CockroachDB) with full fidelity;
            MySQL/MariaDB via <code>pgloader</code>; MongoDB collections → JSONB tables.
          </span>
        </label>
        {isPg && (
          <label className="fld" style={{ flexDirection: 'row', alignItems: 'center', gap: 8, marginTop: 10 }}>
            <input type="checkbox" checked={continuous} onChange={e => setContinuous(e.target.checked)} style={{ width: 'auto' }} />
            <span style={{ margin: 0 }}>
              Continuous replication <span className="muted">— initial copy + streaming for zero-downtime cutover
              (source needs <code>wal_level=logical</code>)</span>
            </span>
          </label>
        )}
        <div className="row" style={{ marginTop: 14 }}>
          <button className="primary" onClick={runDSN} disabled={busy || !source.trim()}>
            {busy ? (continuous && isPg ? 'Starting replication…' : 'Migrating…') : (continuous && isPg ? 'Start continuous replication' : 'Migrate from connection')}
          </button>
        </div>
      </div>

      <div className="panel" style={{ padding: 18, marginTop: 14 }}>
        <h3 style={{ margin: '0 0 10px' }}>From a file</h3>
        <input type="file" accept=".sql,.csv,.json,.ndjson,.jsonl" onChange={e => setFile(e.target.files?.[0] || null)} />
        <p className="hint" style={{ marginTop: 8 }}>
          A <code>.sql</code> dump, a <code>.csv</code>, or a <code>.json</code> export (NoSQL documents → JSONB). Pick a file from anywhere.
        </p>
        <div className="row" style={{ marginTop: 8 }}>
          <button className="primary" onClick={runFile} disabled={busy || !file}>
            {busy ? 'Migrating…' : 'Upload & migrate'}
          </button>
          {busy && <span className="muted" style={{ fontSize: 13 }}>Loading — this can take a moment for large files.</span>}
        </div>
      </div>

      {err && <div className="err">{err}</div>}

      {done && (
        <div className="panel" style={{ marginTop: 14, borderColor: 'var(--green)' }}>
          {done.status === 'replicating' ? (
            <>
              <b style={{ color: 'var(--green)' }}>✓ Continuous replication active → “{done.target}”</b>
              <p className="muted" style={{ margin: '6px 0 10px' }}>
                The initial copy is running and changes now stream continuously. When it has caught up, cut over with{' '}
                <code>vdb import-cutover {done.target}</code>.
              </p>
            </>
          ) : (
            <>
              <b style={{ color: 'var(--green)' }}>✓ Migrated into instance “{done.target}”</b>
              <p className="muted" style={{ margin: '6px 0 10px' }}>{done.tables} table{done.tables === 1 ? '' : 's'} imported.</p>
            </>
          )}
          <div className="row">
            <Link className="ghost" to="/console">Open in SQL Console →</Link>
            <Link className="ghost" to="/ledger">View the ledger →</Link>
          </div>
        </div>
      )}

      <p className="muted" style={{ marginTop: 16, fontSize: 13 }}>
        Large or scripted migrations — and SQLite files — can also use the <code>vdb import</code> CLI, which streams
        a file from anywhere on your machine into a new instance.
      </p>
    </div>
  )
}

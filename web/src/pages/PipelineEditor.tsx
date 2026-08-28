import { useEffect, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import RunConsole, { type Line, type Prog } from '../components/RunConsole'
import {
  createPipeline, getPipeline, listPipelineRuns, runPipelineStream, updatePipeline,
  type PipelineModel, type PipelineTest, type PipelineRun, type PipelineRunResult,
} from '../api'

const TEST_TYPES = ['not_null', 'unique', 'accepted_values', 'row_count_min', 'custom']

export default function PipelineEditor() {
  const { id = 'new' } = useParams()
  const nav = useNavigate()
  const isNew = id === 'new'

  const [pid, setPid] = useState(isNew ? '' : id)
  const [name, setName] = useState('')
  const [source, setSource] = useState('')
  const [models, setModels] = useState<PipelineModel[]>([{ name: '', materialized: 'table', sql: '' }])
  const [tests, setTests] = useState<PipelineTest[]>([])
  const [msg, setMsg] = useState('')
  const [err, setErr] = useState('')

  const [busy, setBusy] = useState(false)
  const [lines, setLines] = useState<Line[]>([])
  const [progress, setProgress] = useState<Prog | null>(null)
  const [result, setResult] = useState<PipelineRunResult | null>(null)
  const [runs, setRuns] = useState<PipelineRun[]>([])

  const loadRuns = (theId: string) => listPipelineRuns(theId).then(r => setRuns(r.runs || [])).catch(() => {})

  useEffect(() => {
    if (isNew) return
    getPipeline(id).then(p => {
      setName(p.name)
      try {
        const spec = JSON.parse(p.spec)
        setSource(spec.source || '')
        setModels(spec.models?.length ? spec.models : [{ name: '', materialized: 'table', sql: '' }])
        setTests(spec.tests || [])
      } catch { /* ignore malformed spec */ }
    }).catch(e => setErr((e as Error).message))
    loadRuns(id)
  }, [id])

  const spec = () => ({ source: source.trim(), models: models.filter(m => m.name.trim() && m.sql.trim()), tests })
  const setModel = (i: number, patch: Partial<PipelineModel>) => setModels(ms => ms.map((m, j) => j === i ? { ...m, ...patch } : m))
  const setTest = (i: number, patch: Partial<PipelineTest>) => setTests(ts => ts.map((t, j) => j === i ? { ...t, ...patch } : t))

  const save = async () => {
    setErr(''); setMsg('')
    try {
      if (pid) { await updatePipeline(pid, name.trim(), spec()); setMsg('Saved.') }
      else { const p = await createPipeline(name.trim(), spec()); setPid(p.id); setMsg('Created.'); nav(`/pipelines/${p.id}`, { replace: true }) }
    } catch (e) { setErr((e as Error).message) }
  }

  const run = async () => {
    if (!pid) { setErr('Save the pipeline before running it.'); return }
    setBusy(true); setLines([]); setProgress(null); setResult(null); setErr(''); setMsg('')
    try {
      const r = await runPipelineStream(pid, e => {
        if (e.type === 'log') setLines(prev => [...prev, { text: e.line }])
        else if (e.type === 'progress') setProgress({ done: e.done, total: e.total, label: e.label })
      })
      setResult(r)
    } catch (e) {
      setLines(prev => [...prev, { text: (e as Error).message, kind: 'err' }])
    } finally {
      setBusy(false)
      loadRuns(pid)
    }
  }

  const loadTemplate = () => {
    setSource('mongodb://mongo-src/shop')
    setModels([
      { name: 'stg_buildings', materialized: 'table', sql: "SELECT _id, name, floors, addr->>'city' AS city, tags\nFROM {{ source('buildings') }}" },
      { name: 'city_counts', materialized: 'table', sql: "SELECT city, count(*) AS buildings, avg(floors)::numeric(6,1) AS avg_floors\nFROM {{ ref('stg_buildings') }}\nGROUP BY city ORDER BY city" },
    ])
    setTests([{ name: 'name not null', type: 'not_null', model: 'stg_buildings', column: 'name' }])
    setMsg('Loaded an example — flattens Mongo docs, then aggregates.')
  }

  return (
    <div className="fade-up" style={{ maxWidth: 1100 }}>
      <div className="row" style={{ justifyContent: 'space-between', alignItems: 'baseline' }}>
        <h1>{!pid ? 'New pipeline' : (name || 'Pipeline')}</h1>
        <button className="ghost" onClick={() => nav('/pipelines')}>← All pipelines</button>
      </div>

      <div className="import-grid">
        {/* left: the pipeline definition */}
        <div>
          <label className="fld"><span>Pipeline name</span>
            <input value={name} onChange={e => setName(e.target.value)} placeholder="analytics" /></label>
          <label className="fld"><span>Source connection string</span>
            <input value={source} onChange={e => setSource(e.target.value)} spellCheck={false}
              placeholder="postgres:// · mysql:// · mongodb://"
              style={{ fontFamily: 'var(--font-mono)', fontSize: 13 }} />
            <span className="hint">Extracted and landed into a <code>raw</code> schema; your models transform it into <code>public</code>.</span>
          </label>

          <div className="row" style={{ justifyContent: 'space-between', alignItems: 'center', marginTop: 10 }}>
            <h3 style={{ margin: 0 }}>Transform models</h3>
            <button className="ghost" onClick={loadTemplate}>Load example</button>
          </div>
          {models.map((m, i) => (
            <div className="panel" key={i} style={{ padding: 12, marginTop: 8 }}>
              <div className="row" style={{ gap: 8 }}>
                <input value={m.name} onChange={e => setModel(i, { name: e.target.value })} placeholder="model name (e.g. stg_orders)" style={{ flex: 1 }} />
                <select value={m.materialized || 'table'} onChange={e => setModel(i, { materialized: e.target.value })}>
                  <option value="table">table</option><option value="view">view</option>
                </select>
                <button className="ghost" onClick={() => setModels(ms => ms.filter((_, j) => j !== i))}>✕</button>
              </div>
              <textarea value={m.sql} onChange={e => setModel(i, { sql: e.target.value })} rows={4} spellCheck={false}
                placeholder="SELECT … FROM {{ source('collection') }}"
                style={{ width: '100%', marginTop: 8, fontFamily: 'var(--font-mono)', fontSize: 12.5 }} />
            </div>
          ))}
          <button className="ghost" style={{ marginTop: 8 }} onClick={() => setModels(ms => [...ms, { name: '', materialized: 'table', sql: '' }])}>+ Add model</button>
          <p className="hint" style={{ marginTop: 6 }}>
            Reference the raw source as <code>{"{{ source('table') }}"}</code> and prior models as <code>{"{{ ref('model') }}"}</code>.
            Flatten nested Mongo docs with jsonb operators, e.g. <code>"gallery"-&gt;0-&gt;&gt;'fileName'</code>.
            Source field names are preserved exactly (case-sensitive) — <b>quote them</b>, e.g. <code>"userId"</code>, <code>"createdAt"</code>.
          </p>

          <h3 style={{ margin: '16px 0 0' }}>Data-quality tests</h3>
          {tests.map((t, i) => (
            <div className="panel" key={i} style={{ padding: 12, marginTop: 8 }}>
              <div className="row" style={{ gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
                <select value={t.type} onChange={e => setTest(i, { type: e.target.value })}>
                  {TEST_TYPES.map(x => <option key={x} value={x}>{x}</option>)}
                </select>
                {t.type !== 'custom' && <input value={t.model || ''} onChange={e => setTest(i, { model: e.target.value })} placeholder="model" style={{ width: 150 }} />}
                {(t.type === 'not_null' || t.type === 'unique' || t.type === 'accepted_values') &&
                  <input value={t.column || ''} onChange={e => setTest(i, { column: e.target.value })} placeholder="column" style={{ width: 120 }} />}
                {t.type === 'row_count_min' && <input type="number" value={t.min ?? 0} onChange={e => setTest(i, { min: Number(e.target.value) })} placeholder="min" style={{ width: 90 }} />}
                <button className="ghost" onClick={() => setTests(ts => ts.filter((_, j) => j !== i))}>✕</button>
              </div>
              {t.type === 'accepted_values' &&
                <input value={(t.values || []).join(', ')} onChange={e => setTest(i, { values: e.target.value.split(',').map(s => s.trim()).filter(Boolean) })}
                  placeholder="value1, value2, …" style={{ width: '100%', marginTop: 8 }} />}
              {t.type === 'custom' &&
                <textarea value={t.sql || ''} onChange={e => setTest(i, { sql: e.target.value })} rows={2} spellCheck={false}
                  placeholder="SELECT … — any rows returned = failures" style={{ width: '100%', marginTop: 8, fontFamily: 'var(--font-mono)', fontSize: 12.5 }} />}
            </div>
          ))}
          <button className="ghost" style={{ marginTop: 8 }} onClick={() => setTests(ts => [...ts, { type: 'not_null', model: '', column: '' }])}>+ Add test</button>

          <div className="row" style={{ marginTop: 18, gap: 10, alignItems: 'center' }}>
            <button className="primary" onClick={save} disabled={busy || !name.trim() || !source.trim()}>Save</button>
            <button className="primary" onClick={run} disabled={busy || !pid}>
              {busy && <span className="spinner" />}{busy ? 'Running…' : 'Run pipeline'}
            </button>
            {msg && <span className="muted" style={{ fontSize: 13 }}>{msg}</span>}
          </div>
          {err && <div className="err">{err}</div>}
        </div>

        {/* right: live console + result + history */}
        <div className="import-console-wrap">
          <RunConsole lines={lines} progress={progress} busy={busy} title="Pipeline run" />
          {result && (
            <div className="panel" style={{ marginTop: 12, borderColor: result.failed ? 'var(--red)' : 'var(--green)' }}>
              <b style={{ color: result.failed ? 'var(--red)' : 'var(--green)' }}>
                {result.failed ? '✗ Completed with test failures' : '✓ Pipeline complete'} → “{result.target}”
              </b>
              <p className="muted" style={{ margin: '6px 0 8px' }}>{result.tables} table{result.tables === 1 ? '' : 's'} in the result instance.</p>
              {result.tests && result.tests.length > 0 && (
                <div style={{ marginBottom: 8 }}>
                  {result.tests.map((tr, i) => (
                    <div key={i} style={{ fontSize: 13, color: tr.passed ? 'var(--green)' : 'var(--red)' }}>
                      {tr.passed ? '✓' : '✗'} {tr.name}{tr.detail ? ` — ${tr.detail}` : ''}
                    </div>
                  ))}
                </div>
              )}
              <div className="row">
                <Link className="ghost" to="/console">Open in SQL Console →</Link>
                <Link className="ghost" to="/ledger">View the ledger →</Link>
              </div>
            </div>
          )}
          {runs.length > 0 && (
            <div className="panel" style={{ marginTop: 12, padding: 0 }}>
              <div className="console-head" style={{ padding: '10px 14px 2px' }}><span>Run history</span></div>
              {runs.map(r => (
                <div key={r.id} className="row" style={{ justifyContent: 'space-between', padding: '8px 14px', borderTop: '1px solid var(--border)', fontSize: 13 }}>
                  <span style={{ color: r.status === 'success' ? 'var(--green)' : r.status === 'running' ? 'var(--muted)' : 'var(--red)' }}>{r.status}</span>
                  <span className="muted">{new Date(r.started * 1000).toLocaleString()} · {r.tables} tables</span>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

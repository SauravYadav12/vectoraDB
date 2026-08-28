import { useEffect, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { listPipelines, deletePipeline, type Pipeline } from '../api'

export default function Pipelines() {
  const [pipelines, setPipelines] = useState<Pipeline[]>([])
  const [err, setErr] = useState('')
  const nav = useNavigate()

  const load = () => listPipelines().then(r => setPipelines(r.pipelines || [])).catch(e => setErr((e as Error).message))
  useEffect(() => { load() }, [])

  const del = async (id: string) => { try { await deletePipeline(id); load() } catch (e) { setErr((e as Error).message) } }

  return (
    <div className="fade-up" style={{ maxWidth: 900 }}>
      <div className="row" style={{ justifyContent: 'space-between', alignItems: 'flex-start' }}>
        <div>
          <h1 style={{ marginBottom: 4 }}>ETL Pipelines</h1>
          <p className="lead" style={{ marginTop: 0 }}>
            Extract from a source, transform it with SQL models, test it, and load the result — into a branch.
          </p>
        </div>
        <button className="primary" onClick={() => nav('/pipelines/new')}>New pipeline</button>
      </div>

      {err && <div className="err">{err}</div>}

      {pipelines.length === 0 ? (
        <div className="panel" style={{ padding: 26, textAlign: 'center' }}>
          <p className="muted" style={{ margin: 0 }}>
            No pipelines yet. Create one to run a real ETL — e.g. extract a MongoDB collection, flatten its
            documents into columns with SQL, aggregate, and assert data quality.
          </p>
        </div>
      ) : (
        <div className="panel" style={{ padding: 0, marginTop: 14 }}>
          {pipelines.map((p, i) => (
            <div key={p.id} className="row"
              style={{ justifyContent: 'space-between', alignItems: 'center', padding: '14px 18px', borderTop: i ? '1px solid var(--border)' : 'none' }}>
              <Link to={`/pipelines/${p.id}`} style={{ fontWeight: 600 }}>{p.name}</Link>
              <div className="row" style={{ gap: 12, alignItems: 'center' }}>
                <span className="muted" style={{ fontSize: 12 }}>updated {new Date(p.updated * 1000).toLocaleDateString()}</span>
                <button className="ghost" onClick={() => del(p.id)}>Delete</button>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

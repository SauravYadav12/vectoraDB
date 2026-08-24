import { useEffect, useState } from 'react'
import { listKeys, createKey, revokeKey, type ApiKey } from '../api'

export default function ApiKeys() {
  const [keys, setKeys] = useState<ApiKey[]>([])
  const [name, setName] = useState('')
  const [fresh, setFresh] = useState<string | null>(null)
  const [err, setErr] = useState('')

  const refresh = () => listKeys().then(r => setKeys(r.keys || [])).catch(e => setErr((e as Error).message))
  useEffect(() => { refresh() }, [])

  const create = async () => {
    setErr('')
    try {
      const r = await createKey(name.trim() || 'key')
      setFresh(r.key)
      setName('')
      await refresh()
    } catch (e) { setErr((e as Error).message) }
  }
  const revoke = async (id: string) => {
    if (!confirm('Revoke this API key? Anything using it stops working.')) return
    try { await revokeKey(id); await refresh() } catch (e) { setErr((e as Error).message) }
  }

  return (
    <div className="fade-up">
      <h1>API keys</h1>
      <p className="muted" style={{ marginTop: -2 }}>
        Use a key as a <code>Bearer</code> token for the API/agent endpoints, or as the
        password when connecting through the proxy.
      </p>

      <div className="row">
        <input placeholder="key name (e.g. ci, laptop)" value={name} onChange={e => setName(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter') create() }} style={{ minWidth: 240 }} />
        <button className="primary" onClick={create}>+ Create key</button>
      </div>
      {err && <div className="err">{err}</div>}

      {fresh && (
        <div className="panel" style={{ marginBottom: 14 }}>
          <b>New key — copy it now, it won’t be shown again:</b>
          <pre style={{ marginTop: 8 }}><code>{fresh}</code>
            <button className="copy" onClick={() => navigator.clipboard?.writeText(fresh)}>copy</button>
          </pre>
        </div>
      )}

      <div className="table-wrap">
        <table>
          <thead><tr><th>Name</th><th>Prefix</th><th>Created</th><th /></tr></thead>
          <tbody>
            {keys.length === 0
              ? <tr><td colSpan={4} className="muted">no keys yet</td></tr>
              : keys.map(k => (
                <tr key={k.id}>
                  <td><b>{k.name}</b></td>
                  <td className="mono muted">{k.prefix}…</td>
                  <td className="muted">{new Date(k.created * 1000).toLocaleString()}</td>
                  <td><div className="actions"><button className="ghost danger" onClick={() => revoke(k.id)}>Revoke</button></div></td>
                </tr>
              ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

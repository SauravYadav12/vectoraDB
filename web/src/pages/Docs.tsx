import { useState } from 'react'
import { Link } from 'react-router-dom'
import { getStatus, getBranches } from '../api'

export default function Docs() {
  const [out, setOut] = useState('// responses appear here')
  const call = async (label: string, fn: () => Promise<unknown>) => {
    setOut(label + ' …')
    try {
      setOut(label + '\n' + JSON.stringify(await fn(), null, 2))
    } catch (e) {
      setOut('error: ' + (e as Error).message)
    }
  }

  return (
    <div className="fade-up">
      <h1>Reference</h1>
      <p className="lead">The <code>vdb</code> CLI, the REST API, and configuration.</p>

      <div className="note" style={{ border: '1px solid var(--border)', borderLeft: '4px solid var(--accent)', borderRadius: 10, padding: '10px 14px', background: 'var(--panel)' }}>
        <strong>New to VectoraDB?</strong> The <Link to="/guide">Developer Guide</Link> walks you from install
        to connecting an app and doing CRUD. This page is the lookup reference.
      </div>

      <h2>CLI reference</h2>
      <table>
        <thead><tr><th>Command</th><th>What it does</th></tr></thead>
        <tbody>
          <tr><td><code>vdb setup</code></td><td>One-time (macOS / Windows): create/start the local VM (WSL2 on Windows) and bring everything up</td></tr>
          <tr><td><code>vdb start</code> · <code>vdb stop</code></td><td>Start / stop the whole stack in the background</td></tr>
          <tr><td><code>vdb status</code></td><td>Servers, primary readiness, and branches</td></tr>
          <tr><td><code>vdb logs [gateway|api]</code></td><td>Print a background server's log</td></tr>
          <tr><td><code>vdb branch create|list|delete|suspend|resume &lt;name&gt;</code></td><td>Manage copy-on-write branches</td></tr>
          <tr><td><code>vdb backup create|list</code> · <code>vdb restore --to &lt;ts|latest&gt;</code></td><td>Time-travel / point-in-time recovery</td></tr>
          <tr><td><code>vdb ha enable|status|failover|disable</code></td><td>High availability (streaming standby + failover)</td></tr>
          <tr><td><code>vdb gateway [--addr :6432] [--idle 2m]</code></td><td>The smart SQL gateway — routes by branch, auto-suspend/resume</td></tr>
          <tr><td><code>vdb serve [--addr :8088]</code></td><td>Agent Branch API — one database per AI agent</td></tr>
          <tr><td><code>vdb user create &lt;email&gt;</code> · <code>vdb apikey create|list|revoke &lt;email&gt;</code></td><td>Accounts &amp; API keys</td></tr>
        </tbody>
      </table>

      <h2>REST API</h2>
      <p className="muted">The control-plane API (default <code>http://localhost:8080</code>). Calls require a session
        cookie or <code>Authorization: Bearer &lt;api-key&gt;</code>. Try the live ones:</p>
      <div className="row">
        <button className="ghost" onClick={() => call('GET /api/status', getStatus)}>GET /api/status</button>
        <button className="ghost" onClick={() => call('GET /api/branches', getBranches)}>GET /api/branches</button>
      </div>
      <pre><code>{out}</code></pre>

      <table>
        <thead><tr><th>Method &amp; path</th><th>Description</th></tr></thead>
        <tbody>
          <tr><td><code>GET /api/status</code></td><td>Primary, counts, HA, storage, servers</td></tr>
          <tr><td><code>GET /api/branches</code></td><td>List branches (state, size, connections)</td></tr>
          <tr><td><code>POST /api/branches</code></td><td>Create a branch — {'{ "name": "qa" }'}</td></tr>
          <tr><td><code>DELETE /api/branches/{'{name}'}</code></td><td>Delete a branch</td></tr>
          <tr><td><code>POST /api/branches/{'{name}'}/suspend|resume</code></td><td>Suspend / resume</td></tr>
          <tr><td><code>POST /api/branches/{'{name}'}/query</code></td><td>Run SQL — {'{ "sql": "…" }'}</td></tr>
          <tr><td><code>POST /agents/{'{id}'}/branch</code> · <code>DELETE …</code></td><td>Agent API (<code>:8088</code>) — create / destroy an agent database</td></tr>
        </tbody>
      </table>

      <h2>Configuration</h2>
      <p className="muted">Set these in the environment before <code>vdb start</code>.</p>
      <table>
        <thead><tr><th>Variable</th><th>Purpose</th></tr></thead>
        <tbody>
          <tr><td><code>VECTORADB_SIGNUP</code></td><td><code>open</code> (default) or <code>closed</code> — allow browser self-signup</td></tr>
          <tr><td><code>VECTORADB_WEB_ORIGIN</code></td><td>Allowed browser origin for the API (default <code>http://localhost:5173</code>)</td></tr>
          <tr><td><code>VECTORADB_PUBLIC_URL</code></td><td>Public base URL (used for OAuth redirects and links)</td></tr>
          <tr><td><code>VECTORADB_GITHUB_CLIENT_ID</code> · <code>_SECRET</code></td><td>Enable “Continue with GitHub” (optional)</td></tr>
          <tr><td><code>VECTORADB_GOOGLE_CLIENT_ID</code> · <code>_SECRET</code></td><td>Enable “Continue with Google” (optional)</td></tr>
          <tr><td><code>VECTORADB_ZPOOL_DEVICE</code> · <code>_SIZE</code></td><td>ZFS pool vdev &amp; size (auto-created on a loopback file if unset)</td></tr>
          <tr><td><code>VECTORADB_LIMA_INSTANCE</code></td><td>macOS: which Lima VM <code>vdb</code> manages</td></tr>
        </tbody>
      </table>
    </div>
  )
}

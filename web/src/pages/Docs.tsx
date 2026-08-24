import { useState } from 'react'
import { getStatus, getBranches } from '../api'

function Code({ children }: { children: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <pre>
      <code>{children}</code>
      <button
        className="copy"
        onClick={() => {
          navigator.clipboard?.writeText(children)
          setCopied(true)
          setTimeout(() => setCopied(false), 1200)
        }}
      >
        {copied ? 'copied' : 'copy'}
      </button>
    </pre>
  )
}

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
      <h1>Documentation</h1>
      <p className="lead">Set up, start, and use VectoraDB.</p>

      <h2>1 · Install</h2>
      <p>One line installs the <code>vdb</code> command. It handles the rest — the Linux VM
        (on macOS), Docker, ZFS, the storage pool, and the image are all set up for you.</p>
      <p><strong>macOS</strong> — needs{' '}
        <a href="https://lima-vm.io" target="_blank" rel="noreferrer">Lima</a> for the local VM:</p>
      <Code>{`brew install lima
curl -fsSL https://raw.githubusercontent.com/SauravYadav12/vectoraDB/main/deploy/install.sh | sh
vdb setup`}</Code>
      <p><strong>Linux</strong>:</p>
      <Code>{`curl -fsSL https://raw.githubusercontent.com/SauravYadav12/vectoraDB/main/deploy/install.sh | sh
sudo vdb start`}</Code>
      <p><code>setup</code> (macOS) / <code>start</code> (Linux) creates the VM, installs Docker
        and ZFS, builds the copy-on-write pool and image, and brings the database, proxy, and
        APIs up — no manual steps.</p>

      <h2>2 · Everyday commands</h2>
      <p>Once installed, <code>vdb</code> is the single command everywhere (on macOS it quietly
        runs inside the VM for you):</p>
      <Code>{`vdb status            # what's running
vdb stop              # stop everything
vdb start             # bring it back up
make web-dev          # run this web UI at http://localhost:5173`}</Code>

      <h2>3 · Branches</h2>
      <Code>{`vdb branch create qa
vdb branch list
vdb branch delete qa`}</Code>
      <p>…or create / suspend / resume / delete them on the <a href="/dashboard">Dashboard</a>.</p>

      <h2>4 · Connect &amp; CRUD</h2>
      <p>Use the <a href="/console">SQL console</a>, or any Postgres client through the proxy
        (database = branch name):</p>
      <Code>{`psql "postgres://vectoradb:vectoradb@127.0.0.1:6432/qa"

CREATE TABLE notes(id serial PRIMARY KEY, body text);
INSERT INTO notes(body) VALUES ('first'),('second');
SELECT * FROM notes;`}</Code>

      <h2>5 · Time-travel · serverless · agents · HA</h2>
      <Code>{`vdb backup create
vdb restore --to latest
vdb branch suspend qa                    # wakes on next connect
curl -X POST localhost:8088/agents/alice/branch # a database per agent
vdb ha enable
vdb ha failover`}</Code>

      <h2>API playground</h2>
      <p className="muted">These call the live control-plane API:</p>
      <div className="row">
        <button className="ghost" onClick={() => call('GET /api/status', getStatus)}>GET /api/status</button>
        <button className="ghost" onClick={() => call('GET /api/branches', getBranches)}>GET /api/branches</button>
      </div>
      <pre><code>{out}</code></pre>

      <h2>API reference</h2>
      <table>
        <thead><tr><th>Method &amp; path</th><th>Description</th></tr></thead>
        <tbody>
          <tr><td><code>GET /api/status</code></td><td>Primary, counts, HA, storage, servers</td></tr>
          <tr><td><code>GET /api/branches</code></td><td>List branches (state, size, connections)</td></tr>
          <tr><td><code>POST /api/branches</code></td><td>Create a branch — {'{ "name": "qa" }'}</td></tr>
          <tr><td><code>DELETE /api/branches/{'{name}'}</code></td><td>Delete a branch</td></tr>
          <tr><td><code>POST /api/branches/{'{name}'}/suspend|resume</code></td><td>Suspend / resume</td></tr>
          <tr><td><code>POST /api/branches/{'{name}'}/query</code></td><td>Run SQL — {'{ "sql": "…" }'}</td></tr>
        </tbody>
      </table>
    </div>
  )
}

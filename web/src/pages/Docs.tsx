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

      <h2>1 · Setup</h2>
      <p>VectoraDB runs inside a Linux VM with ZFS + Docker. On macOS, use{' '}
        <a href="https://lima-vm.io" target="_blank" rel="noreferrer">Lima</a>:</p>
      <Code>{`brew install lima
limactl start --tty=false
lima sudo apt-get update
lima sudo apt-get install -y zfsutils-linux docker.io golang-go
lima sudo truncate -s 30G /var/lib/vectoradb-zpool.img
lima sudo zpool create -f vectoradb /var/lib/vectoradb-zpool.img
lima sudo zfs create vectoradb/branches`}</Code>
      <p>Build the image and CLI inside the VM (repo is auto-mounted):</p>
      <Code>{`lima bash -c 'cd <repo> && sudo docker build -t vectoradb/postgres-walg:16 docker/postgres \\
  && go build -o /tmp/vectoradb ./cmd/vectoradb'`}</Code>

      <h2>2 · Start</h2>
      <p>Bring the DB + APIs up in the background, then run this web app against them:</p>
      <Code>{`lima /tmp/vectoradb start   # control API :8080 · agent API :8088 · proxy :6432
make web-dev                # this UI at http://localhost:5173`}</Code>

      <h2>3 · Branches</h2>
      <Code>{`lima /tmp/vectoradb branch create qa
lima /tmp/vectoradb branch list
lima /tmp/vectoradb branch delete qa`}</Code>
      <p>…or create / suspend / resume / delete them on the <a href="/dashboard">Dashboard</a>.</p>

      <h2>4 · Connect &amp; CRUD</h2>
      <p>Use the <a href="/console">SQL console</a>, or any Postgres client through the proxy
        (database = branch name):</p>
      <Code>{`lima psql "postgres://vectoradb:vectoradb@127.0.0.1:6432/qa"

CREATE TABLE notes(id serial PRIMARY KEY, body text);
INSERT INTO notes(body) VALUES ('first'),('second');
SELECT * FROM notes;`}</Code>

      <h2>5 · Time-travel · serverless · agents · HA</h2>
      <Code>{`lima /tmp/vectoradb backup create
lima /tmp/vectoradb restore --to latest
lima /tmp/vectoradb branch suspend qa          # wakes on next connect
curl -X POST localhost:8088/agents/alice/branch # a database per agent
lima /tmp/vectoradb ha enable
lima /tmp/vectoradb ha failover`}</Code>

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

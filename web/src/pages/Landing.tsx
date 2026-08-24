import { Link } from 'react-router-dom'

const features: [string, string][] = [
  ['⚡ Instant branches', 'Copy-on-write clones of your database in seconds, with near-zero extra space.'],
  ['⏱️ Time-travel / PITR', 'Restore to any point within the archived WAL window.'],
  ['🤖 Database per agent', 'Each AI agent gets an isolated, disposable database over HTTP.'],
  ['💤 Serverless', 'Idle branches suspend; the proxy wakes them on the next connection.'],
  ['☁️ High availability', 'A streaming standby with transparent failover.'],
  ['🔌 One endpoint', 'A wire-protocol proxy routes by database name to any branch.'],
]

export default function Landing() {
  return (
    <>
      <section className="hero">
        <h1>Vectora<span style={{ color: 'var(--accent)' }}>DB</span></h1>
        <p className="lead">
          A serverless PostgreSQL platform — instant copy-on-write branches, time-travel,
          a database per AI agent, and high availability — with native transaction speed.
        </p>
        <div className="cta">
          <Link to="/docs"><button>Get started</button></Link>
          <Link to="/dashboard"><button className="ghost">Open dashboard</button></Link>
          <a href="https://github.com/SauravYadav12/vectoraDB" target="_blank" rel="noreferrer">
            <button className="ghost">GitHub</button>
          </a>
        </div>
      </section>

      <div className="feat">
        {features.map(([t, d]) => (
          <div className="card" key={t}><b>{t}</b><p>{d}</p></div>
        ))}
      </div>

      <h2>How it works</h2>
      <p className="muted" style={{ maxWidth: '72ch' }}>
        VectoraDB runs <b>stock PostgreSQL on local storage</b> for native commit/read latency, and moves
        durability, branching and time-travel <i>off</i> the hot path: <b>ZFS copy-on-write</b> clones for
        instant branches and <b>async WAL archival</b> to object storage. You drive it with one CLI and reach
        it through a single proxy endpoint. See the <Link to="/docs">docs</Link> to set it up, or open the{' '}
        <Link to="/dashboard">dashboard</Link> and <Link to="/console">SQL console</Link>.
      </p>
      <p className="muted" style={{ fontSize: 13 }}>Open source · core AGPL-3.0, clients Apache-2.0.</p>
    </>
  )
}

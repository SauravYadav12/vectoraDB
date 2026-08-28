import { useEffect, useRef } from 'react'

export type Line = { text: string; kind?: 'err' | 'ok' }
export type Prog = { done: number; total: number; label: string }

// RunConsole renders a live log stream (auto-scrolling), an optional determinate
// progress bar, and an idle/working hint. Shared by the migration and pipeline
// pages; each page owns its own submit button + result panel.
export default function RunConsole({ lines, progress, busy, title = 'Console' }: {
  lines: Line[]
  progress: Prog | null
  busy: boolean
  title?: string
}) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => { ref.current?.scrollTo({ top: ref.current.scrollHeight }) }, [lines, progress])
  const pct = progress && progress.total > 0 ? Math.round((progress.done / progress.total) * 100) : 0
  const idle = lines.length === 0 && !busy

  return (
    <div className="import-console-wrap">
      <div className="console-head">
        <span>{title}</span>
        {busy && <span className="muted" style={{ fontSize: 12 }}>running…</span>}
      </div>
      <div className="console" ref={ref}>
        {idle && <div className="muted">The live log will appear here once you run.</div>}
        {lines.map((l, i) => (
          <div key={i} className={l.kind === 'err' ? 'log-err' : l.kind === 'ok' ? 'log-ok' : 'log-line'}>{l.text}</div>
        ))}
      </div>
      {progress && progress.total > 0 && (
        <div className="progress-wrap">
          <div className="progress"><div className="progress-bar" style={{ width: pct + '%' }} /></div>
          <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>{progress.done} / {progress.total} — {progress.label}</div>
        </div>
      )}
      {busy && !progress && <div className="muted" style={{ fontSize: 12, marginTop: 8 }}>Working… (progress shows once models start)</div>}
    </div>
  )
}

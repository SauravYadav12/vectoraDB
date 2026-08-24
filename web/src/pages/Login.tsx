import { useEffect, useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { login, register, providers, oauthUrl, type Providers } from '../api'
import { useAuth } from '../auth-context'

export default function Login() {
  const { user, setUser } = useAuth()
  const nav = useNavigate()
  const [mode, setMode] = useState<'login' | 'register'>('login')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [prov, setProv] = useState<Providers | null>(null)

  useEffect(() => { if (user) nav('/dashboard') }, [user, nav])
  useEffect(() => { providers().then(setProv).catch(() => {}) }, [])

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setErr(''); setBusy(true)
    try {
      const r = mode === 'login' ? await login(email, password) : await register(email, password)
      setUser(r.user)
      nav('/dashboard')
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="authcard fade-up">
      <h1>{mode === 'login' ? 'Log in' : 'Create account'}</h1>
      <p className="muted">Access your VectoraDB dashboard, SQL console, and API keys.</p>

      {prov && (prov.github || prov.google) && (
        <div className="oauth">
          {prov.github && <a className="btn ghost" href={oauthUrl('github')}>Continue with GitHub</a>}
          {prov.google && <a className="btn ghost" href={oauthUrl('google')}>Continue with Google</a>}
          <div className="divider"><span>or</span></div>
        </div>
      )}

      <form onSubmit={submit} className="form">
        <input type="email" placeholder="you@example.com" value={email} onChange={e => setEmail(e.target.value)} required />
        <input type="password" placeholder="password" value={password} onChange={e => setPassword(e.target.value)} required />
        {err && <div className="err">{err}</div>}
        <button className="primary" disabled={busy}>{busy ? '…' : mode === 'login' ? 'Log in' : 'Sign up'}</button>
      </form>

      {prov?.signup !== false && (
        <p className="muted" style={{ marginTop: 14 }}>
          {mode === 'login'
            ? <>No account? <a style={{ cursor: 'pointer' }} onClick={() => setMode('register')}>Create one</a></>
            : <>Have an account? <a style={{ cursor: 'pointer' }} onClick={() => setMode('login')}>Log in</a></>}
        </p>
      )}
    </div>
  )
}

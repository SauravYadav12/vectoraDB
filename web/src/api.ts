// Typed client for the VectoraDB control-plane API (cookie/session auth).
export const API = (import.meta.env.VITE_API_URL as string) || 'http://localhost:8080'
export const AGENT_API = (import.meta.env.VITE_AGENT_API_URL as string) || 'http://localhost:8088'

export type User = { id: number; email: string }
export type HAState = { enabled: boolean; standby: string; streaming: boolean; primary: string }
export type StorageInfo = { used: string; avail: string }
export type Status = {
  mainReady: boolean
  branches: number
  agents: number
  ha: HAState
  storage: StorageInfo
  servers: { gateway: boolean; api: boolean }
}
export type Branch = {
  name: string; primary: boolean; agent: boolean; state: string
  used: string; refer: string; connections: number; port: string
}
export type QueryResult = { columns?: string[]; rows?: unknown[][]; command?: string; error?: string }
export type ApiKey = { id: string; name: string; prefix: string; created: number }
export type Providers = { github: boolean; google: boolean; signup: boolean }

export class ApiError extends Error {
  status: number
  constructor(status: number, msg: string) {
    super(msg)
    this.status = status
  }
}

async function req(method: string, url: string, body?: unknown) {
  const r = await fetch(url, {
    method,
    credentials: 'include',
    headers: body ? { 'Content-Type': 'application/json' } : {},
    body: body ? JSON.stringify(body) : undefined,
  })
  const data = await r.json().catch(() => ({}))
  if (!r.ok) {
    // Session expired mid-use → bounce to login (except when probing /auth/me).
    if (r.status === 401 && !url.endsWith('/auth/me')) {
      if (location.pathname !== '/login') location.assign('/login')
    }
    throw new ApiError(r.status, (data && (data as any).error) || `HTTP ${r.status}`)
  }
  return data
}

// --- auth ---
export const me = () => req('GET', `${API}/auth/me`) as Promise<{ user: User | null }>
export const providers = () => req('GET', `${API}/auth/providers`) as Promise<Providers>
export const login = (email: string, password: string) => req('POST', `${API}/auth/login`, { email, password }) as Promise<{ user: User }>
export const register = (email: string, password: string) => req('POST', `${API}/auth/register`, { email, password }) as Promise<{ user: User }>
export const logout = () => req('POST', `${API}/auth/logout`)
export const oauthUrl = (provider: 'github' | 'google') => `${API}/auth/oauth/${provider}`

// --- api keys ---
export const listKeys = () => req('GET', `${API}/api/keys`) as Promise<{ keys: ApiKey[] }>
export const createKey = (name: string) => req('POST', `${API}/api/keys`, { name }) as Promise<{ key: string; info: ApiKey }>
export const revokeKey = (id: string) => req('DELETE', `${API}/api/keys/${encodeURIComponent(id)}`)

// --- control plane ---
export const getStatus = () => req('GET', `${API}/api/status`) as Promise<Status>
export const getBranches = () => req('GET', `${API}/api/branches`) as Promise<Branch[]>
export const createBranch = (name: string) => req('POST', `${API}/api/branches`, { name })
export const deleteBranch = (name: string) => req('DELETE', `${API}/api/branches/${name}`)
export const suspendBranch = (name: string) => req('POST', `${API}/api/branches/${name}/suspend`)
export const resumeBranch = (name: string) => req('POST', `${API}/api/branches/${name}/resume`)
export const runQuery = (name: string, sql: string) =>
  req('POST', `${API}/api/branches/${name}/query`, { sql }) as Promise<QueryResult>
export const getLedger = (name: string, filters: Record<string, string> = {}) => {
  const qs = new URLSearchParams(Object.entries(filters).filter(([, v]) => v)).toString()
  return req('GET', `${API}/api/branches/${name}/ledger${qs ? '?' + qs : ''}`) as Promise<QueryResult>
}
export const importDB = (source: string, target: string) =>
  req('POST', `${API}/api/import`, { source, target }) as Promise<{ status: string; target: string; tables: number }>
export const importFile = async (file: File, target: string) => {
  const fd = new FormData()
  fd.append('file', file)
  if (target) fd.append('target', target)
  const r = await fetch(`${API}/api/import/file`, { method: 'POST', credentials: 'include', body: fd })
  const data = await r.json().catch(() => ({}))
  if (!r.ok) throw new ApiError(r.status, (data && (data as { error?: string }).error) || `HTTP ${r.status}`)
  return data as { status: string; target: string; tables: number }
}

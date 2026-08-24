// Typed client for the VectoraDB control-plane API.
export const API = (import.meta.env.VITE_API_URL as string) || 'http://localhost:8080'
export const AGENT_API = (import.meta.env.VITE_AGENT_API_URL as string) || 'http://localhost:8088'

export type HAState = { enabled: boolean; standby: string; streaming: boolean; primary: string }
export type StorageInfo = { used: string; avail: string }
export type Status = {
  mainReady: boolean
  branches: number
  agents: number
  ha: HAState
  storage: StorageInfo
  servers: { proxy: boolean; api: boolean }
}
export type Branch = {
  name: string
  primary: boolean
  agent: boolean
  state: string
  used: string
  refer: string
  connections: number
  port: string
}
export type QueryResult = {
  columns?: string[]
  rows?: unknown[][]
  command?: string
  error?: string
}

async function req(method: string, url: string, body?: unknown) {
  const r = await fetch(url, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : {},
    body: body ? JSON.stringify(body) : undefined,
  })
  const data = await r.json().catch(() => ({}))
  if (!r.ok) throw new Error((data && (data as any).error) || `HTTP ${r.status}`)
  return data
}

export const getStatus = () => req('GET', `${API}/api/status`) as Promise<Status>
export const getBranches = () => req('GET', `${API}/api/branches`) as Promise<Branch[]>
export const createBranch = (name: string) => req('POST', `${API}/api/branches`, { name })
export const deleteBranch = (name: string) => req('DELETE', `${API}/api/branches/${name}`)
export const suspendBranch = (name: string) => req('POST', `${API}/api/branches/${name}/suspend`)
export const resumeBranch = (name: string) => req('POST', `${API}/api/branches/${name}/resume`)
export const runQuery = (name: string, sql: string) =>
  req('POST', `${API}/api/branches/${name}/query`, { sql }) as Promise<QueryResult>

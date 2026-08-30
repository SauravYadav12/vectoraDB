// SPDX-License-Identifier: Apache-2.0

/** A thin, dependency-free client for the VectoraDB control-plane REST API.
 * The full API is described by the OpenAPI spec at GET /api/openapi.yaml. */

export interface QueryResult {
  columns?: string[]
  rows?: unknown[][]
  error?: string
}

export interface Branch {
  name: string
  state?: string
  primary?: boolean
  agent?: boolean
  used?: string
  connections?: number
}

export class VectoraDBError extends Error {}

export class VectoraDB {
  constructor(
    private readonly apiKey: string,
    private readonly baseUrl: string = "https://localhost:8080",
  ) {
    this.baseUrl = this.baseUrl.replace(/\/$/, "")
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const res = await fetch(this.baseUrl + path, {
      method,
      headers: {
        Authorization: `Bearer ${this.apiKey}`,
        ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    })
    if (!res.ok) {
      throw new VectoraDBError(`${res.status} ${res.statusText}: ${await res.text()}`)
    }
    const text = await res.text()
    return (text ? JSON.parse(text) : null) as T
  }

  status(): Promise<unknown> {
    return this.request("GET", "/api/status")
  }
  branches(): Promise<Branch[]> {
    return this.request("GET", "/api/branches")
  }
  createBranch(name: string): Promise<unknown> {
    return this.request("POST", "/api/branches", { name })
  }
  deleteBranch(name: string): Promise<unknown> {
    return this.request("DELETE", `/api/branches/${name}`)
  }
  suspend(name: string): Promise<unknown> {
    return this.request("POST", `/api/branches/${name}/suspend`)
  }
  resume(name: string): Promise<unknown> {
    return this.request("POST", `/api/branches/${name}/resume`)
  }
  query(branch: string, sql: string): Promise<QueryResult> {
    return this.request("POST", `/api/branches/${branch}/query`, { sql })
  }
  /** The branch's DDL history — who changed what. */
  ledger(branch = "main", filters: Record<string, string | number> = {}): Promise<QueryResult> {
    const qs = new URLSearchParams(
      Object.entries(filters).map(([k, v]) => [k, String(v)]),
    ).toString()
    return this.request("GET", `/api/branches/${branch}/ledger${qs ? `?${qs}` : ""}`)
  }
  /** Recompute the ledger's hash chain (tamper-evidence). */
  verifyLedger(branch = "main"): Promise<QueryResult> {
    return this.request("GET", `/api/branches/${branch}/ledger/verify`)
  }
}

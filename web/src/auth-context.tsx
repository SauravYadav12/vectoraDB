import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import { me as fetchMe, type User } from './api'

type AuthState = { user: User | null; loading: boolean; setUser: (u: User | null) => void }

const AuthCtx = createContext<AuthState>({ user: null, loading: true, setUser: () => {} })

export const useAuth = () => useContext(AuthCtx)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null)
  const [loading, setLoading] = useState(true)
  useEffect(() => {
    fetchMe()
      .then(r => setUser(r.user))
      .catch(() => setUser(null))
      .finally(() => setLoading(false))
  }, [])
  return <AuthCtx.Provider value={{ user, loading, setUser }}>{children}</AuthCtx.Provider>
}

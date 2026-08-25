import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from 'react'

export type ConfirmOptions = {
  title?: string
  message: ReactNode
  confirmText?: string
  cancelText?: string
  danger?: boolean
}

const ConfirmContext = createContext<(o: ConfirmOptions) => Promise<boolean>>(async () => false)

/** useConfirm() returns an async confirm(options) -> Promise<boolean>, backed by
 *  an in-app modal (replaces window.confirm, which some embedded webviews suppress). */
export function useConfirm() {
  return useContext(ConfirmContext)
}

export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [opts, setOpts] = useState<ConfirmOptions | null>(null)
  const [resolver, setResolver] = useState<{ fn: (ok: boolean) => void } | null>(null)

  const confirm = useCallback(
    (o: ConfirmOptions) => new Promise<boolean>(resolve => {
      setOpts(o)
      setResolver({ fn: resolve })
    }),
    [],
  )

  const close = useCallback((ok: boolean) => {
    resolver?.fn(ok)
    setResolver(null)
    setOpts(null)
  }, [resolver])

  useEffect(() => {
    if (!opts) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') close(false)
      if (e.key === 'Enter') close(true)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [opts, close])

  return (
    <ConfirmContext.Provider value={confirm}>
      {children}
      {opts && (
        <div className="modal-overlay" onClick={() => close(false)}>
          <div className="modal fade-up" role="dialog" aria-modal="true" onClick={e => e.stopPropagation()}>
            {opts.title && <h3>{opts.title}</h3>}
            <div className="modal-body">{opts.message}</div>
            <div className="modal-actions">
              <button className="ghost" onClick={() => close(false)}>{opts.cancelText || 'Cancel'}</button>
              <button className={opts.danger ? 'primary danger' : 'primary'} onClick={() => close(true)} autoFocus>
                {opts.confirmText || 'Confirm'}
              </button>
            </div>
          </div>
        </div>
      )}
    </ConfirmContext.Provider>
  )
}

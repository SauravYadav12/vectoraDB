export type Theme = 'light' | 'dark'

export function getTheme(): Theme {
  const t = document.documentElement.getAttribute('data-theme')
  return t === 'light' ? 'light' : 'dark'
}

export function setTheme(t: Theme) {
  document.documentElement.setAttribute('data-theme', t)
  try {
    localStorage.setItem('theme', t)
  } catch {
    /* ignore */
  }
}

export function toggleTheme(): Theme {
  const next: Theme = getTheme() === 'dark' ? 'light' : 'dark'
  setTheme(next)
  return next
}

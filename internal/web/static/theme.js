// Applies the saved theme before first paint. "auto" follows the system.
try {
  const t = localStorage.getItem('skald-theme');
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
} catch { /* storage blocked: stay on auto */ }

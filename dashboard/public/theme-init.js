// Applies the saved theme before first paint (an external file, so the CSP needs no inline script).
try {
  if (localStorage.getItem('coyote-theme') === 'dark') document.documentElement.dataset.theme = 'dark';
} catch {
  /* storage blocked: light theme */
}

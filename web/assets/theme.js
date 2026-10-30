(() => {
  let preference;
  try {
    preference = localStorage.getItem("hems-theme");
  } catch (_) {
    /* Storage may be disabled. */
  }
  document.documentElement.dataset.theme =
    preference ||
    (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
  matchMedia("(prefers-color-scheme: dark)").addEventListener("change", (e) => {
    let override;
    try {
      override = localStorage.getItem("hems-theme");
    } catch (_) {
      /* No persistence. */
    }
    if (!override) {
      document.documentElement.dataset.theme = e.matches ? "dark" : "light";
      window.dispatchEvent(new Event("themechange"));
    }
  });
})();

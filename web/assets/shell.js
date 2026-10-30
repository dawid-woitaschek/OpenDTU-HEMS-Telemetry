(() => {
  document.querySelectorAll("nav a").forEach((a) => {
    if (a.pathname === location.pathname)
      a.setAttribute("aria-current", "page");
  });
  document.querySelector("#theme-toggle")?.addEventListener("click", () => {
    const theme =
      document.documentElement.dataset.theme === "dark" ? "light" : "dark";
    document.documentElement.dataset.theme = theme;
    try {
      localStorage.setItem("hems-theme", theme);
    } catch (_) {
      /* Theme still works without storage. */
    }
    window.dispatchEvent(new Event("themechange"));
  });
})();

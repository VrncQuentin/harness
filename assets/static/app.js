// Sidebar collapse/expand toggle.
//
// The collapsed state lives on <html> as .sidebar-collapsed so the inline
// head script can restore it before first paint (see layout.html), and in
// localStorage so the choice survives full-page navigation. The toggle
// button lives in the topbar, which is never swapped by htmx partials, so
// its listener stays attached across in-page updates.
(function () {
  "use strict";
  var STORAGE_KEY = "harness.sidebarCollapsed";

  var toggle = document.getElementById("sidebar-toggle");
  if (!toggle) {
    return;
  }
  var icon = toggle.querySelector(".sidebar-toggle-icon");

  function sync() {
    var collapsed = document.documentElement.classList.contains("sidebar-collapsed");
    toggle.setAttribute("aria-expanded", collapsed ? "false" : "true");
    if (icon) {
      icon.textContent = collapsed ? "\u25B6" : "\u25C0";
    }
  }

  toggle.addEventListener("click", function () {
    var collapsed = document.documentElement.classList.toggle("sidebar-collapsed");
    try {
      localStorage.setItem(STORAGE_KEY, collapsed ? "1" : "0");
    } catch (e) {
      // Private browsing or storage disabled - the toggle still works for
      // this page; it just won't persist.
    }
    sync();
  });

  sync();
})();

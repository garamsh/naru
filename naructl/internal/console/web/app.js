// Reloads a page that asks for it, unless the operator is typing, and shapes a typed pairing code.
(function () {
  var seconds = parseInt(document.body.dataset.refresh || "0", 10);
  if (seconds > 0) {
    setInterval(function () {
      var a = document.activeElement;
      if (!a || (a.tagName !== "INPUT" && a.tagName !== "TEXTAREA")) {
        location.reload();
      }
    }, seconds * 1000);
  }
  document.querySelectorAll("input[data-code]").forEach(function (el) {
    el.addEventListener("input", function () {
      var c = el.value.toUpperCase().replace(/[^0-9A-Z]/g, "").slice(0, 8);
      el.value = c.length > 4 ? c.slice(0, 4) + "-" + c.slice(4) : c;
    });
  });
})();

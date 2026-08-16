// Submits the payment hand-over form as soon as the page loads.
//
// This is a file rather than three lines inline because the Content-Security-Policy
// is `script-src 'self'` with no 'unsafe-inline': an inline <script> or an onload
// attribute is blocked, and the shopper would sit on a page waiting for something
// that the browser has silently refused to run. Keeping the policy tight and
// serving this from the binary is the same trade the vendored htmx makes.
//
// The form works without this: it has a real submit button, which is the whole
// mechanism when JavaScript is off.
(function () {
  var form = document.getElementById("gateway-redirect");
  if (form) {
    form.submit();
  }
})();

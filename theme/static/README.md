# Vendored static assets

Served from the binary by `go:embed`, not from a CDN — so the store works offline, the
Content-Security-Policy stays `'self'`, and no third-party origin sits anywhere near the
payment path.

## htmx 2.0.10

- Source: `https://registry.npmjs.org/htmx.org/-/htmx.org-2.0.10.tgz`, `package/dist/htmx.min.js`
- Licence: Zero-Clause BSD (`htmx.LICENSE`)
- Size: 51 238 bytes
- SHA-256: `71ea67185bfa8c98c39d31717c6fce5d852370fcdfd129db4543774d3145c0de`

The bytes were checked against the npm tarball's published `integrity` hash
(`sha512-kdeJe7ZVwaS6QMz/…`) rather than taken from a CDN on trust.

To upgrade, verify the same way — the point of vendoring is lost if the replacement arrives
unchecked:

```sh
V=2.0.11
curl -sSO "https://registry.npmjs.org/htmx.org/-/htmx.org-$V.tgz"
# compare sha512 of the tarball against .dist.integrity from
#   https://registry.npmjs.org/htmx.org/$V
tar -xzOf "htmx.org-$V.tgz" package/dist/htmx.min.js > htmx.min.js
sha256sum htmx.min.js   # record it above
```

Then update the version and hash recorded above. Nothing else needs changing: templates
reference assets as `{{asset "htmx.min.js"}}`, which appends a hash of the file's own
contents to the URL, so replacing the file invalidates every cached copy on its own.

Adding a file here does **not** publish it — `static.go` serves an explicit list, so notes
and licences left in this directory stay unreachable unless they are named there.

## logo.svg and placeholder.svg

Ours, not vendored, and deliberately generic: the logo is an abstract carrier bag
with no text, because the store's name comes from `STORE_NAME` and a name baked into
a logo would be wrong for every adopter but one. Both use `currentColor` or flat
greys so they work on a light or dark page without a second file.

Replace either by putting a file of the same name in `STATIC_DIR` — no rebuild. The
served URL carries a hash of the contents, so the replacement appears immediately
rather than after a cache expires.

## What is served from here

Everything in this directory is embedded, but only extensions in `contentTypes`
(see static.go) are served. This file is embedded and not served, which is the
point: dropping a note in here cannot publish it. The same gate applies to
`STATIC_DIR`, so an `.html` or a `.php` left there is not a URL either.

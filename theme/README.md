# theme/

Your theme. The compose stack mounts this directory into the server and sets
`TEMPLATE_DIR=/theme/templates` and `STATIC_DIR=/theme/static`, with
`THEME_RELOAD=true` — so a file dropped in here takes effect on the next page
refresh, with no restart and no rebuild.

- `templates/` — shaped like
  [`internal/handler/templates/`](../internal/handler/templates): `layouts/`,
  `partials/`, `pages/`, `admin/`, `mail/`. A file at the **same path** as an
  embedded default replaces the definitions in it; every other definition, and
  every other file, keeps falling back. The path is the contract, so a
  `products.gohtml` at the top of this directory is a file nothing looks for.
- `static/` — assets by name, flat. A `styles.css` here shadows the bundled one; a
  `logo.svg` here rebrands the header. New names are served too, so an overridden
  template can reference its own `hero.png`.

Each page is parsed into a template set of its own — the shared partials, its
layout, then the page file. That is what lets a `pages/products.gohtml` here fill
the layout's `nav_extra` block on the catalog and nowhere else. It also means a
page can only call a partial, its own layout, or something it defines itself:
anything two pages need belongs in `partials/`.

Both directories start empty, which means the store looks exactly as it does with
no theme at all. Copy a default out of
[`internal/handler/templates/`](../internal/handler/templates) or
[`internal/handler/static/`](../internal/handler/static) — keeping its
subdirectory — and edit the copy.

Copy only what you are actually changing. A copied file that is not being changed
is a copy that silently stops receiving fixes: it goes on rendering the version of
that page you took, and a later release that corrects it changes nothing for you.

Nothing in here is used unless `TEMPLATE_DIR` / `STATIC_DIR` point at it, so this
directory is safe to leave empty, delete the contents of, or keep in a branch of
your own. See the README's [Theming](../README.md#theming) section.

## What this theme actually changes

Three template files, which is the whole of it:

| File | Changes |
|---|---|
| `templates/partials/document.gohtml` | The favicon, and the stylesheet links, in the `<head>` both layouts share |
| `templates/layouts/public.gohtml` | The storefront header: two dropdowns and a cart button |
| `templates/pages/products.gohtml` | Moves the catalog's filter form out of the page and into the header's Search dropdown, by filling the layout's `nav_extra` block |

Everything else — the product page, the cart, the checkout, the emails, the whole
admin — comes from the defaults on purpose, per the rule above. The copies that
used to sit here are what that rule is about: they were older defaults, and their
only live effect was to undo digital downloads in the confirmation email and demand
a delivery address from a buyer of nothing but files.

`static/` is the rest of the theme, and is now four files: `rivers.css`, `webflow.css`,
and the two Rivers marks.

### Why there is no `styles.css` here

There was one, and it was the rule above being broken in the largest possible way: a
790-line copy of the bundled sheet, of which only 245 lines were Rivers and 545 were
an unchanged copy of a file that had since moved on. Because a `static/` override
shadows by whole file, those 545 lines were not merely stale — they were *authoritative*,
and the rules the copy happened to omit were gone from every page the theme does not
override. Three things were broken by that alone:

- `.visually-hidden` was missing, so screen-reader-only labels rendered on screen in
  the admin product form.
- `.htmx-indicator` was missing while `document.gohtml` still set
  `includeIndicatorStyles:false`, which is the failure that comment warns about: htmx's
  own rules are suppressed, the replacement is absent, and a spinner stays visible.
- `.count`, `.see-all`, `.lede`, `.pager` and `.error-page` were unstyled wherever a
  default template used them.

So the theme now *layers* instead of copying: `styles.css` is linked from the binary,
unshadowed, and `rivers.css` after it carries the whole of what Rivers changes. The
cost is that an override has to say what it wants — under a copy you delete a
declaration, under a layer the base still applies and you must set the property back.
Load order is load-bearing for the same reason, and getting it wrong fails quietly.

### Fonts come from Adobe Fonts, not from here

Helvetica Neue is served by an Adobe Fonts kit, which is why there are no `.woff` files
in this directory any more. There were: eleven faces and a `helvetica.css` that nothing
linked, and that could not have worked if it had been — the faces were `.woff`, which is
not in `static.go`'s `contentTypes`, and they sat in a subdirectory, which the
`STATIC_DIR` walk skips (`if e.IsDir() { continue }`). Hosting them properly would have
meant converting to `.woff2` and flattening the directory; the kit makes both moot.

Three things have to agree, and only the first lives in this directory:

| Where | What |
|---|---|
| `rivers.css` | `--font: helvetica-neue-lt-pro` — the kit's family name, which is what actually applies it |
| `FONT_CSS_URL` | the kit's **CSS** embed, e.g. `https://use.typekit.net/abc1def.css` |
| `FONT_ORIGINS` | `https://use.typekit.net,https://p.typekit.net` — two hosts, because the kit's stylesheet and the `.woff2` files it points at are served from different ones |

Use the CSS embed from the Web Project panel, not the JavaScript one: the loader
snippet needs `script-src` widened and a nonce for the inline `<script>` and `<style>`
it injects, which this project does not support.

The last two travel together. `FONT_CSS_URL` on an origin `FONT_ORIGINS` does not list
is refused at boot, rather than blocked by the CSP in the browser where the only symptom
is a console warning and a page in the fallback font. Setting neither is also fine — the
page falls back to `sans-serif` and nothing else changes.

The cost, stated plainly because it is the one this project otherwise refuses to pay:
the kit puts a third-party origin on every page, the checkout included, and widens
`style-src` and `font-src` past `'self'` to do it. A self-hosted `.woff2` would keep the
CSP shut and the payment path free of anyone else's server. This is a deliberate trade
of that for the licence and the convenience, not an oversight.

Two token choices flatten distinctions the bundled sheet draws, which is a design call
rather than a defect: `--ink-soft` is set to the same value as `--ink`, so every `.hint`
reads at full strength rather than muted, and `--accent` is near-black, so the `paid`
badge on an order is not distinguishable from a neutral one.

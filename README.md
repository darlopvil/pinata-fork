# Pinata (fork)

A light, JavaScript-free frontend for Pinterest: search, open pins, save bookmarks, all without loading anything from Pinterest in your browser.

This is darlopvil's fork of [Pinata by gigirassy](https://codeberg.org/gigirassy/pinata), with its own features and fixes, listed below. Issues and suggestions are welcome in this repository.

## What it does

- **No JavaScript, no database.** A single Go binary in a Docker image of a few megabytes; settings and bookmarks live in the browser, in an encrypted cookie.
- **Pinterest never sees the browser.** The server fetches searches, pins and images; pages load nothing from other sites.
- Search, pin pages, bookmarks, reverse image search and an optional [image proxy backend](https://codeberg.org/gigirassy/image-proxy) to recompress images.

## Compared to the original

### New features

- **Pin pages** at `/pin/{id}`, also in Pinterest's `<slug>--<id>` form, with title, description, source link, pinner, board, date and saves. Search results link to them too: logged-out searches return pin IDs Pinterest won't resolve, so their cards carry the pin's data in a signed link.
- **Save buttons that don't reload the page**, on cards, pin pages and searches. The heart saves the pin, and the homepage shows saved pins as thumbnails that open their pin page.
- **Settings page** behind a gear icon on every page: accent color, image size, reverse search engine, Animate GIFs, and bookmark export and import. Saving goes back to the page you came from.
- **Reverse image search** with Yandex (default), Google Lens, Bing, SauceNAO or TinEye, instead of TinEye only.
- **Animated GIFs** in search results, or still images if you prefer (Animate GIFs: never).
- **Downloaded images are named after the pin**, instead of `image_proxy`.
- **Lighter thumbnails**, resized from Pinterest's own JPEG variants instead of the full originals: on 10 test results, 148 KB instead of 20.1 MB with GIFs shown still.
- **Image placeholders**: real alt text and each pin's dominant color while images load.
- **Error pages** that say what happened: Pinterest rate-limiting the instance, Pinterest errors, no results, unknown pages.
- A favicon.

### Bug fixes

Security:

- Any website could wipe your bookmarks or reset your settings with a hidden form or a plain link. Cross-site requests are now blocked, and only POST changes anything.
- The instance could be used to redirect visitors to any other site, through a form parameter (open redirect).
- No Content-Security-Policy or other security headers; cookies were set without `Secure`, and settings cookies without `HttpOnly`.

Behavior:

- A Pinterest error, rate limiting included, showed as an empty results page, so a blocked instance looked like a search with no results.
- A result without an image showed the previous result's image.
- Pages appeared unstyled for a second or two after "Next page" in Firefox.
- Searches longer than 21 characters in Chinese, Japanese or Korean bounced back to the homepage: the length limit counted bytes, not characters.
- The 30th bookmark was silently lost: the cookie outgrew what browsers accept.
- HEIC images failed to show in search results.
- Unknown pages answered with the homepage instead of a 404.
- The homepage claimed no data reaches Pinterest and that the instance owner can never see what you view. It now explains what actually happens (see Privacy).
- Firefox warned about an unsupported CSS property on every page.

## How to run

### Docker Compose (recommended)

- Clone this repo: `git clone https://github.com/darlopvil/pinata-fork.git pinata && cd pinata`
- Copy the example: `cp compose.example.yml compose.yml`. `compose.yml` is git-ignored, so your settings and key never get committed.
- Set `PINATA_BOOKMARK_KEY` (see below).
- The image proxy is optional: remove the `pinata-proxy` service and `PINATA_IMAGE_BACKEND` to fetch images directly.
- `docker compose up -d --build`
- To update: `git pull && docker compose up -d --build`

### Go

Requires Go 1.26 or newer. Pinata listens on port 8080.

- `go build -trimpath -ldflags="-s -w" -o pinata ./main.go`
- `export PINATA_BOOKMARK_KEY=$(head -c 32 /dev/urandom | base64)`
- `./pinata`

## Configuration

| Variable | What it does |
| --- | --- |
| `PINATA_BOOKMARK_KEY` | 32 random bytes in base64: `head -c 32 /dev/urandom \| base64`. Enables bookmarks and signs pin links. Without it, bookmarks are off and pin links stop working whenever the server restarts. Changing it invalidates existing bookmarks and pin links. |
| `PINATA_IMAGE_BACKEND` | Base URL of an image proxy backend, e.g. `http://pinata-proxy:8081`. Empty: images are fetched directly. |
| `PINATA_DISABLE_REVERSE` | `1`, `true` or `yes` hides reverse image search. |

### Behind a reverse proxy

- Pass `X-Forwarded-Proto: https`, so cookies get the `Secure` flag. Nginx Proxy Manager and most proxies do this by default.
- Responses that set the bookmarks cookie stay under 4 KB of headers, the default upstream buffer of nginx (`proxy_buffer_size`), so no proxy tuning is needed.

## Privacy

Pinterest only talks to the server running Pinata. That server does see the searches and pins requested through it, like any proxy: use an instance you trust, or host your own. Pinata doesn't log requests (only startup messages and failures talking to Pinterest) and stores nothing on disk; bookmarks are encrypted with the instance's key and kept in your browser.

## License

[ISC](LICENSE), as the original Pinata. Copyright of the original code belongs to its author; the fork's changes are under the same license.

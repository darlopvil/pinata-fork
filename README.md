# Pinata (fork)

A light, JavaScript-free frontend for Pinterest: search, open pins, save bookmarks, all without loading anything from Pinterest in your browser.

This is darlopvil's fork of [Pinata by gigirassy](https://codeberg.org/gigirassy/pinata). It is developed on its own, without pull requests to the original; issues are welcome here.

## Features

- **No JavaScript, no database.** A single Go binary in a Docker image of a few megabytes; settings and bookmarks live in the browser, in an encrypted cookie.
- **Pinterest never sees the browser.** The server fetches searches, pins and images; pages load nothing from other sites (strict Content-Security-Policy, no referrer).
- **Pin pages** with title, description, source link, pinner, board and date, for pins opened from search results or from Pinterest pin URLs.
- **Bookmarks** for searches and pins, shown as thumbnails on the homepage, saved and removed without reloading the page. Export and import as JSON.
- **Reverse image search** with Yandex (default), Google Lens, Bing, SauceNAO or TinEye.
- **Settings** behind the gear icon: accent color, image size, animated or still GIFs, reverse search engine.
- Downloaded images are named after the pin.
- Optional [image proxy backend](https://codeberg.org/gigirassy/image-proxy) to recompress images.

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

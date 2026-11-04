# Pinata (fork)

A light frontend for Pinterest, with no JavaScript save for idea pin videos: search, open pins, profiles and boards, follow them by RSS and save bookmarks, all without loading anything from Pinterest in your browser.

This is darlopvil's fork of [Pinata by gigirassy](https://codeberg.org/gigirassy/pinata), with its own features and fixes, listed below. Issues and suggestions are welcome in this repository.

## What it does

- **No JavaScript (one exception, below), no database.** A single Go binary in a Docker image of a few megabytes; settings and bookmarks live in the browser, in an encrypted cookie.
- **Pinterest never sees the browser.** The server fetches searches, pins and images; pages load nothing from other sites.
- Search, pin pages, profiles and boards, videos, RSS feeds, bookmarks, reverse image search and an optional [image proxy backend](https://codeberg.org/gigirassy/image-proxy) to recompress images.

## Compared to the original

### New features

- **Pin pages** at `/pin/{id}`, also in Pinterest's `<slug>--<id>` form, with title, description, source link, pinner, board, date and saves. Search results link to them too: logged-out searches return pin IDs Pinterest won't resolve, so their cards carry the pin's data in a signed link.
- **Profiles and boards** at their Pinterest paths (`/<user>/`, `/<user>/<board>/`): a profile's boards with their covers and its pins (quick saves outside any board included), both paginated, and a board's pins, page by page. Without an account Pinterest serves boards only in part (24 pins of a 460-pin board, in one measurement), and Pinata says how many it shows; with the optional account (below), close to the whole board. Pin pages link to the pinner's profile and to the board.
- **More like this** on pin pages opened by their Pinterest ID (Pinterest links, short links), with further pages: the pins Pinterest relates to that one, fetched alongside the pin so the page doesn't wait longer. Pins opened from search results, boards or other related pins don't get them: Pinterest only gives related pins for a pin's numeric ID, and those pins only come with opaque ones.
- **RSS feeds** of searches and boards, to follow them from a feed reader: an RSS link next to their title, also announced to readers in the page, at the same address plus `?format=rss`. A feed is the first page of results; it comes from the same cache as the page and asks readers to check hourly, so following a search costs Pinterest a few requests an hour at most. Entries link to the instance's pin pages, with the instance's thumbnail.
- **Short links** (`pin.it/<code>`) at `/pin.it/<code>`: Pinata asks Pinterest once where the link leads, caches the answer for good and opens that pin, board or profile, without the tracking parameters Pinterest adds (`invite_code`, `sender`). Pinterest links pasted in the search box, short or full and from any Pinterest domain, open here too.
- **Save buttons that don't reload the page**, on cards, pin pages and searches. The heart saves the pin, and the homepage shows saved pins as thumbnails that open their pin page.
- **Settings page** behind a gear icon on every page: accent color, image size, reverse search engine, Animate GIFs, and bookmark export and import. Saving goes back to the page you came from.
- **Reverse image search** with Yandex (default), Google Lens, Bing, SauceNAO or TinEye, instead of TinEye only.
- **Video pins play** on their pin page, through the instance, with nothing downloaded until you press play and nothing playing by itself. Search results only mark them with a ▶ badge, so they stay as fast as before. Regular video pins play as MP4 with no JavaScript; idea pins only exist as HLS with separate audio, so they play through hls.js (see below).
- **Animated GIFs** in search results, or still images if you prefer (Animate GIFs: never).
- **Downloaded images are named after the pin**, instead of `image_proxy`, and pin pages have a **Download** button next to "Full size".
- **Lighter thumbnails**, resized from Pinterest's own JPEG variants instead of the full originals: on 10 test results, 148 KB instead of 20.1 MB with GIFs shown still.
- **Optional disk cache**: images are kept until a size cap, search pages, profiles, boards, related pins and feeds for 15 minutes, pins for an hour and short links for good, so reloads, going back, feed readers and other devices don't ask Pinterest again.
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
| `PINATA_CACHE_DIR` | Directory for the disk cache, e.g. a mounted volume at `/cache`. Empty: no cache, nothing is written to disk. |
| `PINATA_CACHE_IMAGES_MB` | Size cap of the image cache, in MB (default 1024). The least recently used images go first. |
| `PINATA_CACHE_API_MB` | Size cap of the cache of search pages, profiles, boards and pins, in MB (default 64). |
| `PINATA_PINTEREST_SESSION`, `PINATA_PINTEREST_CSRF` | The `_pinterest_sess` and `csrftoken` cookies of an optional throwaway account, used for board pages only (see Pinterest account). Empty: no account. |

### Monitoring

`/healthz` answers in plain text, without asking Pinterest anything:

```
pinata: ok
account: on
pinterest: ok
```

- `account`: `none` without an account; `on` while it logs in; `off` once Pinterest stopped taking it (set new cookies and restart).
- `pinterest`: `rate-limited` when Pinterest's latest answer to the instance was a 429, `ok` again as soon as it answers normally. It reflects real traffic, so a monitor that also opens a search now and then keeps it current.

### Behind a reverse proxy

- Pass `X-Forwarded-Proto: https`, so cookies get the `Secure` flag. Nginx Proxy Manager and most proxies do this by default.
- Responses that set the bookmarks cookie stay under 4 KB of headers, the default upstream buffer of nginx (`proxy_buffer_size`), so no proxy tuning is needed.

## JavaScript

Pinata serves no JavaScript, except on pin pages with an idea pin video: Pinterest keeps those only as HLS with the audio in a separate stream, which browsers can't play from a plain `<video>`. Those pages load [hls.js](https://github.com/video-dev/hls.js) (Apache-2.0, `static/hls.min.js`, version 1.7.3 from npm) and a short script of Pinata's (`static/video.js`, Apache-2.0), both served by the instance and labelled for GNU LibreJS. The Content-Security-Policy allows scripts on those pages only. With JavaScript off, the video shows its poster and a note.

## Pinterest account (optional)

Without an account, Pinterest serves some of its pages only in part. A frontend that works in parts isn't the goal, so an instance can give Pinata a throwaway account. Like JavaScript, it's used only where nothing else works, and as little as possible: today, for board pages only. Searches, pins, profiles and related pins stay logged out, so Pinterest never sees searches tied to the account.

Measured on a 460-pin board: 24 pins logged out, 411 with an account. Board pins then come with their Pinterest ID, so their pin pages also get "More like this".

- Create a throwaway account, log in with a browser, and copy two of its cookies for pinterest.com: `_pinterest_sess` and `csrftoken`.
- Set them as `PINATA_PINTEREST_SESSION` and `PINATA_PINTEREST_CSRF`, in an env file only the instance can read.
- If the session stops working (it expired, or Pinterest banned the account), Pinata goes on logged out and logs it once: set new cookies and restart.

What it costs: the boards opened through the instance are tied to that account, and Pinterest may ban it.

## Privacy

Pinterest only talks to the server running Pinata. That server does see the searches and pins requested through it, like any proxy: use an instance you trust, or host your own. Pinata doesn't log requests (only startup messages, failures talking to Pinterest and cache hit counts); bookmarks are encrypted with the instance's key and kept in your browser. Without the disk cache it writes nothing to disk. With it, the server keeps images it has served, and for a short while the search pages and pins it fetched; file names are hashes, so they don't show what was searched, but the cached pages themselves contain the results. With the optional account, Pinterest sees the boards opened through the instance tied to that account. The instance also asks crawlers to stay away (`robots.txt`): every page they followed would be a request to Pinterest, counting toward the rate limit that blocks the instance for everyone.

## License

[ISC](LICENSE), as the original Pinata. Copyright of the original code belongs to its author; the fork's changes are under the same license.

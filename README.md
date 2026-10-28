# Pinata is Yet Another Pinterest Frontend.

<img src="https://codeberg.org/gigirassy/pinata/raw/branch/main/screenies/pinata.png" alt="pinata-screenshot">

Self-explanatory. It's built with the idea of being light as possible, and not strain the end user's system while being fun.

The Docker image, built by you, ranges between 5-7 MB, and the entire workings depend on a single main.go.

* Pinata by itself takes very little memory to run, about 14MB of memory with docker image when active, and 6MB when idle.
* The frontend uses no Javascript, which makes it friendly to low-end systems like netbooks!
* Pinata does not need a database to function, and all customization and saved bookmarks is done with encrypted cookies. No logs are kept in the container.
* Memory isn't a concern? Bring your own image proxy backend if you'd like! This allows you to compress images for the end user (making it even lighter), rotate ips, and do other fun stuff. An example that auto-compresses images: https://codeberg.org/gigirassy/image-proxy/

Despite being so light, Pinata supports bookmarks via cookies (encrypted!) and can be made a PWA on your phone.

**Contribute to <a href="https://codeberg.org/gigirassy/pinata/">this repo on Codeberg</a>!**

## How to run

### Docker Compose (recommended)

* Clone this repo: `git clone https://github.com/darlopvil/pinata-fork.git pinata && cd pinata`
* Copy the example: `cp compose.example.yml compose.yml`. `compose.yml` is git-ignored, so your settings and key never get committed.
* To enable bookmarks, set `PINATA_BOOKMARK_KEY` to the output of `head -c 32 /dev/urandom | base64`.
* The image proxy is optional: remove the `pinata-proxy` service and `PINATA_IMAGE_BACKEND` to fetch images directly.
* `docker compose up -d --build`
* To update: `git pull && docker compose up -d --build`

### Go

Requires Go 1.26 or newer. Pinata listens on port 8080.

* `go build -trimpath -ldflags="-s -w" -o pinata ./main.go`
* (optional) `export PINATA_BOOKMARK_KEY=$(head -c 32 /dev/urandom | base64)` to enable bookmarks
* `./pinata`

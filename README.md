# Golang Web Server

**What is this?** A program that sends visitors the files of your website (pages, pictures, CSS, and the rest). It is written in Go, a programming language, and uses the [Fiber v3](https://gofiber.io) web library.

This server is meant to run on port 80 behind Cloudflare. Cloudflare connects to your server on port 80, and Cloudflare handles HTTPS for visitors.

**Benchmark:** 508,184 requests per second (home page with Brotli, 256 connections). See [Benchmarks](#benchmarks).

## Contents

- [Which version should I use?](#which-version-should-i-use)
- [Quick start (5 minutes)](#quick-start-5-minutes)
- [Keep it running](#keep-it-running)
- [Settings](#settings)
- [Using Cloudflare](#using-cloudflare)
- [Other systems](#other-systems)
- [Troubleshooting / FAQ](#troubleshooting--faq)
- [Benchmarks](#benchmarks)
- [License](#license)

## Which version should I use?

The project has eight folders. Each folder is a full copy of the same server. The only difference is the settings block at the top of that folder's `main.go`. Run one folder.

**Use `cache-all` for most sites.** It makes text files smaller, and it tells browsers to keep every file, including pages, for a year. The [quick start](#quick-start-5-minutes) uses this folder. After you change a page, restart the server and purge Cloudflare's cache, or visitors can keep the old page for up to a year.

Pick another folder when you want a different tradeoff:

| Folder | What it does |
|---|---|
| `cache-all` | Shrinks text files, and tells browsers to keep every file, including pages, for a year without asking again. |
| `cache-assets-only` | Shrinks text files. Browsers ask about pages on every visit, and keep pictures, CSS, and JavaScript for a year. |
| `cache-1-minute` | Shrinks text files. Browsers keep pages for 1 minute and other files for a year. |
| `no-cache` | Sends files as they are, and tells browsers to keep nothing. Handy while you are testing. |
| `no-cache+compression` | Shrinks text files, and tells browsers to keep nothing. |
| `cache-all-no-compression` | Tells browsers to keep every file for a year. It does not shrink files, so startup uses less CPU. |
| `cache-assets-only-no-compression` | Browsers ask about pages on every visit and keep other files for a year. It does not shrink files. |
| `cache-1-minute-no-compression` | Browsers keep pages for 1 minute and other files for a year. It does not shrink files. |

Words used above:

- **Shrink** means Brotli, a format that makes text smaller. HTML, CSS, JavaScript, JSON, and SVG are shrunk. Pictures, video, audio, and zip files are sent as they are. Serving starts once the files are read. The strongest compression then finishes in the background.
- **Keep for a year** means the browser may reuse its saved copy and skip the download.
- **Ask on every visit** means the browser checks with the server. If nothing changed, the server answers `304 Not Modified` and sends no file body.
- An **ETag** is a fingerprint of a file, and it is what makes that check work. The six folders whose names start with `cache-` send ETags. `no-cache` and `no-cache+compression` do not.
- Every folder can send a piece of a file (a byte range). Video players need that to skip ahead.

Folders that shrink files also mark responses so Cloudflare will not compress them a second time. Folders that do not shrink files leave text open, so Cloudflare can compress it. On those folders, Cloudflare's Bot Fight Mode can strip ETags from pages. Turn Bot Fight Mode off when you want `304` answers for pages.

## Quick start (5 minutes)

These steps are for Ubuntu and Debian. Paste each block into the terminal. They use `sudo`, which runs a command as the administrator. If you are already the `root` user, `sudo` still works.

The steps use the `cache-all` folder. For another version, use that folder's name wherever these steps say `cache-all`.

### 1. Install Go

This downloads Go 1.27.4, the version named in `go.mod`, from the official site, checks the file, and installs it.

```bash
sudo apt update && sudo apt install -y curl ca-certificates git && bash <<'EOF'
set -euo pipefail
case "$(uname -m)" in
  x86_64) arch=amd64; sum=7226736c2339719cda28fbf3a54d71aa63bcefe376e98cef00034268dab24840 ;;
  aarch64) arch=arm64; sum=3392582988a273a87ab5043ee1b2c9c17a2c3c00580bf9326c47a2e0aaaeac92 ;;
  *) echo "This installer knows amd64 (x86_64) and arm64 (aarch64). This machine is $(uname -m). Install Go 1.27.4 or newer from https://go.dev/dl/ and continue at step 2."; exit 1 ;;
esac
curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go1.27.4.linux-${arch}.tar.gz"
echo "${sum}  /tmp/go.tgz" | sha256sum -c -
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf /tmp/go.tgz
echo 'export PATH=/usr/local/go/bin:$PATH' | sudo tee /etc/profile.d/go.sh >/dev/null
EOF
export PATH="/usr/local/go/bin:$PATH"
hash -r
go version
```

The last line should start with `go version go1.27.4`. A new login already has Go on the `PATH`. If a later terminal says `go: command not found`, see [go: command not found](#go-command-not-found).

### 2. Download the project

This copies the code into your home folder.

```bash
cd "$HOME"
git clone https://github.com/dumptcp/golangwebserver.git
```

### 3. Open the cache-all folder

`cache-all` is the version from the table above.

```bash
cd "$HOME/golangwebserver/cache-all"
```

### 4. Build the program

This makes one program named `webserver` in the current folder. Stay in `cache-all`. Go finds `go.mod` in the folder above. The first build downloads libraries, so the machine needs internet.

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o webserver .
```

`CGO_ENABLED=0` means the build does not need a C compiler. When the command finishes, `ls` shows a file named `webserver`.

### 5. Put your website in the default folder

The server reads a folder called `ROOT`. When you do not set `ROOT`, that folder is `/root/www`, and the home page is `index.html` inside it. This creates a one-page test site there.

```bash
sudo mkdir -p /root/www
sudo tee /root/www/index.html >/dev/null <<'EOF'
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>It works</title>
</head>
<body>
  <h1>It works</h1>
</body>
</html>
EOF
```

Replace that file with your real site when you are ready. A file at `/root/www/style.css` is served at `/style.css`. See [How to upload your real site](#how-to-upload-your-real-site).

### 6. Start the server

A **port** is the number that picks which program receives a connection. This server listens on port 80 only. That is the port Cloudflare uses to reach your machine. Leave this terminal open. The program stops when you press Ctrl+C or close the terminal.

Port 80 needs the administrator. Start it with `sudo`:

```bash
sudo ./webserver
```

You are ready when the output includes `listening on :80` and a line that starts with `loaded` and mentions `/root/www`.

If you are not root, stay on port 80. `sudo ./webserver` is the usual way. The other way is to allow this program to use port 80, then run it as your user. Run `setcap` again after every rebuild, because a new file loses that permission:

```bash
sudo apt install -y libcap2-bin
sudo setcap cap_net_bind_service=+ep ./webserver
./webserver
```

`/root/www` is readable by root, so `sudo ./webserver` is the command that serves the test page from step 5. With `setcap`, put the site in a folder your user can read and start with `ROOT="$HOME/www" ./webserver`. That is still port 80.

### 7. Check that it works

Open a second terminal, because the first one is busy running the server. This asks for the home page on port 80.

```bash
curl -fsS http://127.0.0.1/
```

You should see `It works`. Visitors use `https://yourdomain` after you finish [Using Cloudflare](#using-cloudflare). Checking the raw server IP in a browser is not how this site is meant to be opened.

## Keep it running

The quick start stops when you close the terminal. Pick one of the two options below.

### screen (simplest)

`screen` keeps a terminal session alive after you disconnect. Stop the test server first (Ctrl+C in its terminal), or the port is already taken.

```bash
sudo apt install -y screen
cd "$HOME/golangwebserver/cache-all"
screen -S website
```

You are now inside the screen session. Start the server the same way as in the quick start, on port 80:

```bash
sudo ./webserver
```

If `sudo` asks for a password, type it. When you see `listening on :80`, detach: press Ctrl+A, release those keys, then press D. You are back at a normal prompt, and the server is still running. If you used `setcap` instead of `sudo`, run `./webserver` here (or `ROOT="$HOME/www" ./webserver` when the site is in your home folder).

Come back to it later:

```bash
screen -r website
```

If that says the session is attached, use `screen -d -r website`. List sessions with `screen -ls`. To stop the server, come back to the session and press Ctrl+C. It stops accepting new visitors, finishes downloads already in progress (up to 30 seconds), then exits.

### systemd (starts on boot)

systemd is the program that starts services when Linux boots. This is optional. The service runs as `www-data`, the usual web account on Ubuntu, so the site folder must be readable by that account. `/root/www` is not. These commands copy the program and the test site into place:

```bash
sudo cp "$HOME/golangwebserver/cache-all/webserver" /usr/local/bin/webserver
sudo mkdir -p /var/www/site
sudo cp -a /root/www/. /var/www/site/
sudo chown -R www-data:www-data /var/www/site
```

If your files are in `$HOME/www` instead of `/root/www`, copy from that folder.

Create the service file:

```bash
sudo nano /etc/systemd/system/webserver.service
```

Paste this, then save and close nano (Ctrl+O, Enter, Ctrl+X):

```ini
[Unit]
Description=Static web server
After=network.target

[Service]
User=www-data
Group=www-data
WorkingDirectory=/var/www/site
Environment=ROOT=/var/www/site
AmbientCapabilities=CAP_NET_BIND_SERVICE
ExecStart=/usr/local/bin/webserver
Restart=on-failure
RestartSec=2
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
```

What the lines do:

- `Environment=ROOT` is the website folder. Change it if your files live somewhere else.
- `AmbientCapabilities` lets `www-data` listen on port 80.
- `ExecStart` is the program. After every rebuild, copy the new `webserver` file to `/usr/local/bin/webserver` again.
- `Restart=on-failure` starts it again if it crashes, waiting 2 seconds first.
- `LimitNOFILE` lets it hold many connections at once.

Turn it on and start it now:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now webserver
sudo systemctl status webserver --no-pager
```

`enable` means "also start at boot". Useful commands:

```bash
sudo systemctl restart webserver
sudo systemctl stop webserver
sudo journalctl -u webserver -n 50 --no-pager
```

Stop the screen copy before starting the service, so they are not both trying to use port 80.

## Settings

An environment variable is a name and a value you put in front of the command. It affects that run only.

`sudo` drops those variables unless you pass them with `env`:

```bash
sudo env ROOT=/var/www/site CACHE_MEMORY=256M ./webserver
```

| Name | Default | What it does | Example |
|---|---|---|---|
| `ROOT` | `/root/www` | Folder of website files. The home page is `index.html` in this folder. `/about/` is the file `about/index.html`. | `ROOT="$HOME/www"` |
| `CACHE_MEMORY` | 1/4 of usable memory | RAM the server may use for file contents and their Brotli copies. A plain number is bytes. `K`, `M`, `G`, or `T` (optional `B` or `iB`) means kibibytes, mebibytes, gibibytes, or tebibytes, so `512M` is 512 MiB. Files that do not fit are read from disk when someone asks, with no Brotli copy. | `CACHE_MEMORY=512M` |
| `MAX_IN_MEMORY` | `2097152` (2 MB) | One file larger than this is always read from disk, even when `CACHE_MEMORY` has room. The value is bytes, with no `M` suffix. | `MAX_IN_MEMORY=8388608` |
| `MAX_CONNECTIONS` | unset (no cap) | How many connections are served at once. Unset or `0` means no cap. A positive number is the cap: past it, the extra connection gets `503 Service Unavailable`, `Retry-After: 1`, and an empty body, then it is closed. The server keeps accepting. | `MAX_CONNECTIONS=10000` |
| `KEEP_ALIVE` | `120` | Seconds to keep an idle connection open so the next request can reuse it. 120 keeps a flood through Cloudflare from holding hundreds of thousands of idle connections open. A 910 second timeout let about 300,000 stay open, and when they all closed at once the leftover sockets cut off SSH. The smallest value is 1. | `KEEP_ALIVE=60` |
| `GOMEMLIMIT` | unset | A Go runtime setting. If you set it, the server leaves Go's memory limit alone. If you leave it unset, the server asks Go to stay under half of usable memory. Use Go's suffixes: `KiB`, `MiB`, `GiB`, `TiB`. | `GOMEMLIMIT=1GiB` |

**Usable memory** is the smaller of the machine's RAM and any container memory limit. If the server cannot read either number, it assumes 1 GiB. `CACHE_MEMORY` defaults to one quarter of that. There is no connection cap unless `MAX_CONNECTIONS` is set above 0. On Linux, macOS, and FreeBSD the server raises its soft open-file limit up to the hard limit at startup. Windows has no such limit, so that step is skipped.

At startup the server prints the usable memory, the connection cap (or unlimited), memory limit, cache budget, and keep-alive, then how many files were loaded into memory and how many are served from disk. It opens port 80 before it reads files, so a failure to listen shows up immediately. The server listens on port 80. Do not point it at another port.

The server reads the site once, at startup. Restart it after you add or edit files. Build again only after you change a `.go` file.

A few limits are fixed in the code (they are not settings): a request header and a request body may each be up to 16 KB, the server allows 10 seconds to read a request and 30 seconds to write a response. On shutdown (Ctrl+C or `kill`), downloads already in progress get up to 30 seconds to finish.

## Using Cloudflare

This is part of the setup. Put [Cloudflare](https://www.cloudflare.com/) in front of the site. Cloudflare connects to your server on port 80. Visitors get HTTPS from Cloudflare, not from this program.

In the Cloudflare dashboard:

1. Add a DNS A record for your domain pointing at this machine's IP, and leave it **Proxied** (orange cloud).
2. Set **SSL/TLS → Overview** to **Flexible**. This server speaks plain HTTP on port 80. **Full** and **Full (strict)** try to reach your server over HTTPS, and visitors get an error page.
3. Turn on **Always Use HTTPS** (SSL/TLS → Edge Certificates).
4. Turn on **Smart Tiered Cache** (Caching → Tiered Cache).

`cache-all` tells caches to keep HTML for a year. Cloudflare does not cache HTML on its own. To make it follow the header this server sends, add a Cache Rule (Caching → Cache Rules): **Eligible for cache**, Edge TTL **Use cache-control header if present**, Browser TTL **Respect origin**. After you change the site, purge the cache (Caching → Configuration → Purge Everything). To cache `.json` as well, add a rule whose file extension is `json`, with the same TTL settings.

### The firewall file

`cloudflare.conf`, in the project folder next to `cache-all`, is a Linux firewall file for nftables (the `nft` firewall tool). Loading it allows only Cloudflare's addresses to open port 80, allows SSH on the port you choose, and drops other new incoming connections. Loopback stays open, so `curl http://127.0.0.1/` on the machine still works. The file adds a table named `inet webfw`. It leaves your other firewall tables alone, and it does not change forwarded traffic, so Docker on the same machine keeps working.

Load it after the DNS record is proxied. A browser pointed at the server's IP is then blocked, because that visitor is not Cloudflare.

> **Keep your SSH port correct, or you can lock yourself out.** `SSH_PORT` must be the port you actually use for SSH. The usual port is 22. If you connect on another port, put that number in the file before you load it. A wrong value drops new SSH sessions. After loading, open a second SSH session and confirm it works. Only then close the first one.

`WEB_PORT` is already 80. Leave it there. This server listens on port 80, and the firewall must open that same port. Change `SSH_PORT` only.

```bash
sudo apt install -y nftables
sudo nano "$HOME/golangwebserver/cloudflare.conf"
```

```text
define SSH_PORT = 22
define WEB_PORT = 80
```

Load it and check:

```bash
sudo nft -f "$HOME/golangwebserver/cloudflare.conf"
sudo nft list ruleset
```

The table is gone after a reboot unless you save it. If `/etc/nftables.conf` is empty, this can be the boot config:

```bash
sudo cp "$HOME/golangwebserver/cloudflare.conf" /etc/nftables.conf
sudo systemctl enable nftables
```

If `/etc/nftables.conf` already has other tables, paste `cloudflare.conf` into that file instead of replacing it.

The address lists in the file come from [cloudflare.com/ips](https://www.cloudflare.com/ips/). They rarely change. When Cloudflare publishes a new address, update both lists and load the file again.

Remove only this firewall:

```bash
sudo nft delete table inet webfw
```

Every response also sends `Strict-Transport-Security`, which tells browsers to use HTTPS for this domain and all its subdomains for a year. That matches a site whose DNS is proxied by Cloudflare. If a subdomain is not on HTTPS, remove `; includeSubDomains` from `vHSTS` in `main.go`, build again, and restart.

## Other systems

The same `cache-all` folder builds on macOS, FreeBSD, and Windows. `cloudflare.conf` is Linux nftables, so skip that file on these systems. The server still listens on port 80. Install Go 1.27.4 or newer from [go.dev/dl](https://go.dev/dl/) and check with `go version`.

**macOS.** Use the `.pkg` installer from that page (Apple silicon or Intel). Then clone, build, and run with the same commands as Linux. Start it with `sudo ./webserver` so it can listen on port 80.

**FreeBSD.** Install the official tarball, or install the `lang/go` package and confirm `go version` says 1.27.4 or newer. Then use the same clone, build, and run commands. Start it with `sudo ./webserver` so it can listen on port 80.

**Windows.** Install Go from the `.msi` on that page. In PowerShell, from the `cache-all` folder:

```powershell
$env:CGO_ENABLED = "0"
go build -trimpath -ldflags="-s -w" -o webserver.exe .
New-Item -ItemType Directory -Force -Path C:\www | Out-Null
Set-Content -Path C:\www\index.html -Value "<!doctype html><h1>It works</h1>"
$env:ROOT = "C:\www"
.\webserver.exe
```

It listens on port 80. Open `http://127.0.0.1/`. If Windows refuses that port, run PowerShell as Administrator and start it again. There is no `setcap` and no `nft` on Windows.

## Troubleshooting / FAQ

### Port 80: permission denied

The program prints `cannot listen on :80: permission denied`. Port 80 needs the administrator. Start it with sudo:

```bash
sudo ./webserver
```

Or grant this program permission to use port 80, then run it as your user. Install the tool if the command is missing, and run `setcap` again after every rebuild (a new file loses the permission):

```bash
sudo apt install -y libcap2-bin
sudo setcap cap_net_bind_service=+ep ./webserver
ROOT="$HOME/www" ./webserver
```

### The port is already in use

The program prints `cannot listen on :80: the port is in use`. Another program already has port 80. This server has to use port 80, so stop the other program. See who has it:

```bash
sudo ss -tlnp sport = :80
```

Apache and nginx are the usual occupants. Stop them and keep them from starting again:

```bash
sudo systemctl stop apache2 nginx
sudo systemctl disable apache2 nginx
```

If the name in the `ss` output is an old `webserver`, stop that copy (Ctrl+C in its terminal, or `sudo systemctl stop webserver`) and start this one.

### 404 or no files found

`no files found under /root/www` means that folder is missing or empty. The server exits. Create the folder and put `index.html` directly inside it, then start again.

A `404` page means the server is running, and the address you asked for is not a file under `ROOT`. The home page must be named `index.html` and must sit in `ROOT` itself, not in an extra subfolder. `/about` and `/about/` both serve `about/index.html`.

To use your own missing-page file, put `404.html` in the top of the site folder. Inside it, start links with `/` (for example `/style.css`), because that page is shown at many addresses.

Names that start with a dot are skipped (`.env`, `.git`, and similar). The folder `.well-known/` is served, which is what certificate checks use. Symbolic links are skipped. The site folder itself may be a link; that one link is followed.

### Out of memory

`CACHE_MEMORY` defaults to about a quarter of usable memory (see [Settings](#settings)). Files that do not fit, and any single file bigger than 2 MB, are read from disk instead of being loaded at startup. A large site still starts.

If the terminal says `Killed`, or the machine runs out of memory while compressing, start again with a smaller budget:

```bash
sudo env CACHE_MEMORY=128M ./webserver
```

`128M` is 128 MiB. On a small VPS, `64M` or `128M` is a reasonable start. You can also lower the per-file cap, in bytes: `MAX_IN_MEMORY=524288` is 512 KB.

### A firewall is blocking the port

If `curl http://127.0.0.1/` on the server prints the page, the program is fine. Visitors reach it through Cloudflare, on port 80.

This server uses `cloudflare.conf` as its firewall. If you loaded it, only Cloudflare can reach port 80. A direct visit to the IP fails on purpose. Test with curl to `127.0.0.1` on the server. See [The firewall file](#the-firewall-file) to install nftables, load the file, and keep it after a reboot.

To remove that firewall:

```bash
sudo nft delete table inet webfw
```

### How to stop

In the terminal where it is running, press Ctrl+C. It finishes work already in progress (up to 30 seconds), then exits. `kill` on the process does the same.

Inside screen: `screen -r website`, then Ctrl+C.

With systemd:

```bash
sudo systemctl stop webserver
```

### How to update

The server reads files only when it starts. After you change HTML, CSS, or images, restart it. You do not need to build again for that.

After `git pull`, or after you edit a `.go` file, build again and restart:

```bash
cd "$HOME/golangwebserver"
git pull
cd cache-all
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o webserver .
```

Then start it the way you did before. If you use systemd, copy the new binary over the old one and restart the service:

```bash
sudo cp "$HOME/golangwebserver/cache-all/webserver" /usr/local/bin/webserver
sudo systemctl restart webserver
```

With `cache-all`, purge Cloudflare after a content change, or visitors can keep the old page for up to a year.

### How to upload your real site

On the server, files belong in `/root/www` (or whatever you set as `ROOT`). `index.html` in that folder is the home page. From your own computer, with the site in the current folder:

```bash
scp -r . root@YOUR_SERVER_IP:/root/www/
```

SFTP apps such as FileZilla, WinSCP, or MobaXterm work too. Connect to the server IP, sign in as the same user you use for SSH, and upload into `/root/www`. Then restart the server.

### Can I run PHP or a contact form?

This server sends files that are already on disk. It answers `GET` and `HEAD`. A form or script that sends `POST`, `PUT`, or `DELETE` gets `405 Method Not Allowed`. PHP, Python, and databases need a different program.

### Fonts, scripts, or videos from other sites

The server sends a Content-Security-Policy that allows pages to load things from your own site. A font, script, image, or embed hosted somewhere else is blocked until you allow that host. Open `main.go`, find `contentSecurityPolicy`, add the other site on the matching line, build again, and restart.

| You want | Which part | What to add |
|---|---|---|
| Google Fonts | `style-src` and `font-src` | `https://fonts.googleapis.com` on `style-src`, `https://fonts.gstatic.com` on `font-src` |
| A script from a CDN | `script-src` | the CDN, for example `https://cdn.jsdelivr.net` |
| Images or video hosted elsewhere | `img-src` or `media-src` | the site they come from |
| A YouTube embed | a new `frame-src` part | `https://www.youtube.com` |
| Analytics or an API | `connect-src`, and `script-src` for its script | the site, for example `https://api.example.com` |

### go: command not found

The install put Go on the `PATH` for new logins. In the terminal you already have open:

```bash
source /etc/profile.d/go.sh
go version
```

If that file is missing, step 1 did not finish. Run it again. `go version` should start with `go version go1.27.4`.

## Benchmarks

On Tuesday Nov 24, 2026 (4:16–4:22 AM PT), the phoenixNAP dedicated server in Chicago (Intel Xeon E-2388G, 8 cores / 16 threads, 62 GiB RAM, Ubuntu 26.04.1, kernel 7.0, tuned sysctl and BBR) built each server's `cache-all` from main and ran it alone on 127.0.0.1 with its default settings (keep-alive 910s, no logging), serving the same 7-file site. The builds were [rawrustwebserver](https://github.com/dumptcp/rawrustwebserver) `9832e7b`, [rustwebserver](https://github.com/dumptcp/rustwebserver) `4ec454c`, and this repo `d63bff2`. The server was pinned with `taskset` to CPUs 0–3 and 8–11 (4 cores / 8 threads) and the load tool, oha 1.17.0, to CPUs 4–7 and 12–15, so the two never competed for a core. After a 3s warmup, oha ran three 10-second tests at 256 connections (Rust 1.100.0, Go 1.27.4), and the median is reported. The 4096-connection test is one 10-second run. "Home + Brotli" sends `Accept-Encoding: br` (3,128-byte response). "Home plain" sends `Accept-Encoding: identity` (13,976-byte response); the Nov 22 run left the header out, but oha then sends `gzip, compress, deflate, br` by default, so that row was really Brotli too.

**Requests/sec** (RPS) is how many requests came back each second (higher is better). **Typical latency** is p50, and **slow latency** is p99. **Transfer** is response bytes per second. Memory is peak RSS.

[rawrust](https://github.com/dumptcp/rawrustwebserver) is the no-framework Rust server, [actix](https://github.com/dumptcp/rustwebserver) is Actix, and golang is this server.

### Quick winner

Home page with Brotli, 256 connections.

| Server | Requests/sec | Avg latency | Typical latency | Slow latency (p99) | Transfer | Memory |
|---|---:|---:|---:|---:|---:|---:|
| **rawrust** | **715,813** | **0.356 ms** | **0.312 ms** | **0.498 ms** | **2.24 GB/s** | **8.4 MB** |
| actix | 599,974 | 0.425 ms | 0.402 ms | 0.763 ms | 1.88 GB/s | 11.3 MB |
| golang | 508,184 | 0.502 ms | 0.399 ms | 1.788 ms | 1.59 GB/s | 17.8 MB |

### All numbers

Requests per second. Bold is the fastest in that row.

| What | Connections | rawrust | actix | golang |
|---|---:|---:|---:|---:|
| Home + Brotli | 256 | **715,813** | 599,974 | 508,184 |
| Home plain | 256 | **472,350** | 459,861 | 429,407 |
| Home + Brotli | 4096 | **386,559** | 366,650 | 371,058 |

Home plain, 256 connections, typical latency / slow latency (p99) and transfer: rawrust 0.494 ms / 0.760 ms, **6.60 GB/s**; actix 0.563 ms / **0.723 ms**, 6.43 GB/s; golang **0.468 ms** / 2.177 ms, 6.00 GB/s.

### 4096 connections

Home page with Brotli, one 10-second run.

| Server | Requests/sec | Avg latency | Typical latency | Slow latency (p99) | Transfer | Memory |
|---|---:|---:|---:|---:|---:|---:|
| rawrust | **386,559** | **10.57 ms** | **10.63 ms** | **11.97 ms** | **1.21 GB/s** | **24.1 MB** |
| actix | 366,650 | 11.15 ms | 11.11 ms | 14.13 ms | 1.15 GB/s | 78.6 MB |
| golang | 371,058 | 11.01 ms | 10.84 ms | 20.13 ms | 1.16 GB/s | 44.0 MB |

---

## License

MIT — see [LICENSE](LICENSE).

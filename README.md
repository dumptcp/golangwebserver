# Golang Web Server

A fast static file server written in Golang (Fiberv3/gofiber.io). run a few commands, and your site is live.

---

## Step 1 — Install Go + update your server

Run these in your terminal, one at a time:

```bash
sudo apt update
```

```bash
sudo apt upgrade -y
```

```bash
sudo apt autoremove -y
```

> ⚠️ **Warning:** the reboot below will take your server offline for a minute or two while it restarts.

```bash
sudo reboot
```
When the server is back you can continue

```bash
sudo apt install snapd
```

```bash
sudo snap install go --classic
```

Reconnect to your server after it comes back up. You now have Go installed and your system is up to date.

---

## Step 2 — Pick a version

Each folder is a complete, standalone server. Pick **one**:

| Folder | Compression | HTML Cache | Asset Cache | Best For |
|---|---|---|---|---|
| `cache-assets-only/` | ✅ Yes | None (always fresh) | 1 year | **Recommended.** Most sites |
| `cache-all/` | ✅ Yes | 1 year | 1 year | Sites that never change |
| `cache-1-minute/` | ✅ Yes | 1 minute | 1 year | News sites, blogs |
| `no-cache/` | ❌ No | None | None | Dev, debugging |
| `no-cache+compression/` | ✅ Yes | None | None | Fresh + small downloads |
| `cache-all-no-compression/` | ❌ No | 1 year | 1 year | Cached, low CPU |
| `cache-assets-only-no-compression/` | ❌ No | None (always fresh) | 1 year | Fresh HTML, low CPU |
| `cache-1-minute-no-compression/` | ❌ No | 1 minute | 1 year | Frequent edits, low CPU |

**Legend:**
- **Compression ✅** = Files are pre-compressed into Zstd, Brotli, and Gzip at startup. Smaller downloads, faster page loads.
- **Compression ❌** = Files are served as-is. Larger downloads, but lower CPU usage on the server.
- **HTML Cache** = How long browsers keep your `index.html`. "None" means every visit gets the latest version.
- **Asset Cache** = How long browsers keep images, CSS, and JS. "1 year" is standard.

---

## Step 3 — Add your website files

make sure webserver backend is in /root while frontend files is in the /root/www directory

---

## Step 4 — Golang Dependencies

Then run these two commands, one at a time:

```bash
go mod init webserver
```

```bash
go mod tidy
```

`go mod tidy` downloads everything the server needs. If it asks for anything, just run it again.

---

## Step 5 — Open the firewall for Cloudflare

Load the included `cloudflare.conf`:

```bash
sudo nft -f cloudflare.conf
```

Verify it's active:

```bash
sudo nft list ruleset
```

## Step 6 — Run the server

Start the server:

```bash
sudo go run main.go
```

---

## Notes + Tips and Tricks

To clear nftables when you want:

```bash
sudo nft flush ruleset
```

Can help with gain more performance

```bash
ulimit -n 999999;ulimit -u unlimited;ulimit -e unlimited;ulimit -r unlimited
```

---
- This is a **static** server. It serves HTML, CSS, JS, images, and other files. It does **not** run PHP, Python, or any other server-side code.
- If you update a file while using `cache-all`, you may need to clear your browser cache or Cloudflare cache to see the change.
- `cache-assets-only` is the safest choice for most people.

## License

MIT — see [LICENSE](LICENSE).

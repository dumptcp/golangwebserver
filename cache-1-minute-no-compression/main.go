package main

import (
	"fmt"
	"hash/fnv"
	"io/fs"
	"log"
	"math"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
)

var (
	siteRoot   = envStr("ROOT", "/root/www")
	listenAddr = ":" + envStr("PORT", "80")
)

const (
	htmlCacheControl  = "public, max-age=60, no-transform"
	assetCacheControl = "public, max-age=31536000, immutable, no-transform"

	// Fiber's default of 4096 answers "431 Request Header Fields Too Large" to
	// any visitor whose cookies and headers add up to more than 4 KB.
	readBufferSize = 16 * 1024

	// How long one response may take to reach the visitor. The clock runs for
	// the whole response, so the old value of 15 seconds cut large files
	// (video, downloads) off for everyone who needed longer than that.
	writeTimeout = time.Hour

	// This policy only allows resources from your own site. If your pages load
	// fonts, scripts, images, video or embeds from another site, add that site
	// to the matching line (see the README), or they will be blocked.
	contentSecurityPolicy = "default-src 'self'; " +
		"base-uri 'self'; " +
		"form-action 'self'; " +
		"frame-ancestors 'none'; " +
		"img-src 'self' data: blob:; " +
		"media-src 'self' data: blob:; " +
		"style-src 'self' 'unsafe-inline'; " +
		"script-src 'self' 'unsafe-inline' 'wasm-unsafe-eval'; " +
		"worker-src 'self' blob:; " +
		"font-src 'self'; " +
		"connect-src 'self'; " +
		"object-src 'none'; " +
		"upgrade-insecure-requests"

	permissionsPolicy = "accelerometer=(), autoplay=(), camera=(), " +
		"display-capture=(), encrypted-media=(), fullscreen=(self), " +
		"geolocation=(), gyroscope=(), magnetometer=(), microphone=(), " +
		"midi=(), payment=(), picture-in-picture=(self), publickey-credentials-get=(), " +
		"screen-wake-lock=(), sync-xhr=(), usb=(), xr-spatial-tracking=()"
)

type file struct {
	ctype, cc, etag string
	body            []byte
}

var files = map[string]*file{}

// Content types for the extensions a website normally uses. Anything not listed
// falls back to the operating system's MIME table, which a minimal server may
// not have, so everything a browser is picky about is spelled out here.
var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".htm":         "text/html; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".json":        "application/json; charset=utf-8",
	".webmanifest": "application/json; charset=utf-8",
	".map":         "application/json; charset=utf-8",
	".svg":         "image/svg+xml; charset=utf-8",
	".txt":         "text/plain; charset=utf-8",
	".xml":         "application/xml; charset=utf-8",
	".csv":         "text/csv; charset=utf-8",
	".md":          "text/markdown; charset=utf-8",
	".vtt":         "text/vtt; charset=utf-8",
	".wasm":        "application/wasm",
	".pdf":         "application/pdf",
	".zip":         "application/zip",

	".ico":  "image/x-icon",
	".png":  "image/png",
	".apng": "image/apng",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
	".bmp":  "image/bmp",
	".jxl":  "image/jxl",
	".heic": "image/heic",

	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	".eot":   "application/vnd.ms-fontobject",

	".mp4":  "video/mp4",
	".m4v":  "video/x-m4v",
	".mov":  "video/quicktime",
	".webm": "video/webm",
	".ogv":  "video/ogg",
	".3gp":  "video/3gpp",
	".ts":   "video/mp2t",
	".m3u8": "application/vnd.apple.mpegurl",
	".mpd":  "application/dash+xml",

	".mp3":  "audio/mpeg",
	".m4a":  "audio/mp4",
	".aac":  "audio/aac",
	".ogg":  "audio/ogg",
	".oga":  "audio/ogg",
	".opus": "audio/ogg",
	".wav":  "audio/wav",
	".flac": "audio/flac",
	".weba": "audio/webm",
}

func main() {
	load(siteRoot)

	if len(files) == 0 {
		log.Fatalf("no files found under %s", siteRoot)
	}
	log.Printf("loaded %d files from %s", len(files), siteRoot)

	app := fiber.New(fiber.Config{
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   writeTimeout,
		IdleTimeout:    960 * time.Second, // must outlast Cloudflare's 900s connection reuse, or it causes 520s
		ReadBufferSize: readBufferSize,
	})
	app.All("/*", serve)
	log.Printf("listening on %s", listenAddr)
	log.Fatal(app.Listen(listenAddr, fiber.ListenConfig{
		DisableStartupMessage: true,
		// Fiber listens on IPv4 only unless told otherwise. "tcp" accepts IPv4
		// and IPv6, and still starts on a server that has no IPv6.
		ListenerNetwork: fiber.NetworkTCP,
	}))
}

func serve(c fiber.Ctx) error {
	// Fiber gives the raw path, so "/my%20file.html" would never match a file
	// named "my file.html". Decode it first; keep the raw path if it is malformed.
	p := c.Path()
	if u, err := url.PathUnescape(p); err == nil {
		p = u
	}

	if strings.HasSuffix(p, "/index.html") {
		c.Set("Location", redirectTarget(strings.TrimSuffix(p, "index.html"))+query(c))
		c.Set("Cache-Control", assetCacheControl)
		return c.SendStatus(fiber.StatusMovedPermanently)
	}

	key := strings.TrimPrefix(p, "/")
	if key == "" || strings.HasSuffix(key, "/") {
		key += "index.html"
	}

	f := files[key]
	if f == nil {
		// "/about" when the site has an "about" folder: send the browser to
		// "/about/", which is where that folder's index.html is served.
		if !strings.HasSuffix(p, "/") && files[key+"/index.html"] != nil {
			c.Set("Location", redirectTarget(p+"/")+query(c))
			c.Set("Cache-Control", assetCacheControl)
			return c.SendStatus(fiber.StatusMovedPermanently)
		}
		return notFound(c)
	}

	if inm := c.Get("If-None-Match"); inm != "" && strings.Contains(inm, f.etag) {
		c.Set("ETag", f.etag)
		c.Set("Cache-Control", f.cc)
		return c.SendStatus(fiber.StatusNotModified)
	}

	body := f.body
	h := &c.Response().Header

	// Byte ranges: what video and audio players use for seeking, and what
	// Safari needs to play media at all.
	h.Set("Accept-Ranges", "bytes")
	if rng := c.Get("Range"); rng != "" && c.Method() == fiber.MethodGet {
		// If-Range means "only send the part if the file is still this
		// version"; when it is not, the whole file is sent instead.
		if ir := c.Get("If-Range"); ir == "" || ir == f.etag {
			switch start, end, res := parseRange(rng, len(body)); res {
			case rangeOK:
				h.Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(end)+"/"+strconv.Itoa(len(body)))
				c.Status(fiber.StatusPartialContent)
				body = body[start : end+1]
			case rangeUnsatisfiable:
				c.Set("Content-Range", "bytes */"+strconv.Itoa(len(body)))
				c.Set("Cache-Control", "no-store")
				return c.SendStatus(fiber.StatusRequestedRangeNotSatisfiable)
			}
		}
	}

	h.Set("ETag", f.etag)
	h.Set("Cache-Control", f.cc)
	securityHeaders(c)

	h.SetContentType(f.ctype)
	return c.Send(body)
}

// securityHeaders adds the headers that tell the browser how to protect the
// visitor. They go on every page, including the 404 page.
func securityHeaders(c fiber.Ctx) {
	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	c.Set("X-Frame-Options", "DENY")
	c.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	c.Set("Content-Security-Policy", contentSecurityPolicy)
	c.Set("Permissions-Policy", permissionsPolicy)
}

// notFound answers 404. If the site has a 404.html in its top folder, that page
// is shown; otherwise the visitor gets a plain "Not Found".
func notFound(c fiber.Ctx) error {
	c.Set("Cache-Control", "no-store")
	f := files["404.html"]
	if f == nil {
		return c.SendStatus(fiber.StatusNotFound)
	}
	securityHeaders(c)
	c.Response().Header.SetContentType(f.ctype)
	return c.Status(fiber.StatusNotFound).Send(f.body)
}

// redirectTarget turns a decoded directory path into a Location that can only
// ever point back at this site. Every unusual byte is percent-encoded again, so
// a browser cannot reinterpret a "\" or a tab as part of the address, and the
// leading slashes are collapsed to one, so "//other.example/" stays a path on
// this site instead of becoming another host.
func redirectTarget(dir string) string {
	return "/" + strings.TrimLeft((&url.URL{Path: dir}).EscapedPath(), "/")
}

// query returns the request's query string with its "?", so that a redirect
// keeps it: "/about?ref=x" goes to "/about/?ref=x". It is passed on only when
// it is made of plain visible characters; anything else is left out.
func query(c fiber.Ctx) string {
	q := c.Request().URI().QueryString()
	if len(q) == 0 {
		return ""
	}
	for _, b := range q {
		if b <= ' ' || b >= 0x7f {
			return ""
		}
	}
	return "?" + string(q)
}

const (
	rangeIgnore        = iota // not a single valid byte range: send the whole file
	rangeOK                   // send bytes start..end
	rangeUnsatisfiable        // starts past the end of the file: answer 416
)

// parseRange reads a Range header holding one byte range: "bytes=0-99",
// "bytes=100-" or "bytes=-500" (the last 500 bytes). Anything else, including a
// list of several ranges, is ignored, which HTTP allows: the client then simply
// receives the whole file.
func parseRange(header string, size int) (start, end, result int) {
	const unit = "bytes="
	if size == 0 || len(header) < len(unit) || !strings.EqualFold(header[:len(unit)], unit) {
		return 0, 0, rangeIgnore
	}
	spec := strings.TrimSpace(header[len(unit):])
	if strings.Contains(spec, ",") {
		return 0, 0, rangeIgnore
	}
	first, last, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, rangeIgnore
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)

	if first == "" { // "-N": the last N bytes
		n, ok := parsePos(last)
		if !ok {
			return 0, 0, rangeIgnore
		}
		if n == 0 {
			return 0, 0, rangeUnsatisfiable
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, rangeOK
	}

	start, ok = parsePos(first)
	if !ok {
		return 0, 0, rangeIgnore
	}
	end = size - 1
	if last != "" {
		e, ok := parsePos(last)
		if !ok || e < start {
			return 0, 0, rangeIgnore
		}
		if e < end {
			end = e
		}
	}
	if start >= size {
		return 0, 0, rangeUnsatisfiable
	}
	return start, end, rangeOK
}

// parsePos parses a plain run of digits. Numbers too large to be a position in
// any file are capped rather than rejected.
func parsePos(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		d := s[i]
		if d < '0' || d > '9' {
			return 0, false
		}
		if n > (math.MaxInt-9)/10 {
			n = math.MaxInt
		} else {
			n = n*10 + int(d-'0')
		}
	}
	return n, true
}

func load(root string) {
	// WalkDir does not follow symlinks, not even for the folder it starts in.
	// Resolve that one, so a site folder that is itself a symlink still loads.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		// Hidden files and folders are left out. The site folder itself is
		// taken whatever it is called, also when it is given as ".".
		if path != root && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		add(filepath.ToSlash(rel), body)
		return nil
	})
}

func add(rel string, body []byte) {
	ext := strings.ToLower(filepath.Ext(rel))
	ctype := contentTypes[ext]
	if ctype == "" {
		ctype = mime.TypeByExtension(ext)
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	cc := assetCacheControl
	if ext == ".html" || ext == ".htm" {
		cc = htmlCacheControl
	}

	h := fnv.New64a()
	_, _ = h.Write(body)
	etag := fmt.Sprintf(`"%016x"`, h.Sum64())

	files[rel] = &file{ctype: ctype, cc: cc, etag: etag, body: body}
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

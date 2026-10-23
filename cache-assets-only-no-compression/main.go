package main

import (
	"fmt"
	"hash/fnv"
	"io/fs"
	"log"
	"math"
	"mime"
	"net"
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
	listenAddr = net.JoinHostPort(envStr("HOST", ""), envStr("PORT", "80"))
)

const (
	htmlCacheControl  = "no-cache, must-revalidate"
	assetCacheControl = "public, max-age=31536000, immutable"

	readBufferSize = 16 * 1024

	writeTimeout = time.Hour

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
		IdleTimeout:    960 * time.Second,
		ReadBufferSize: readBufferSize,
	})
	app.All("/*", serve)
	log.Printf("listening on %s", listenAddr)
	log.Fatal(app.Listen(listenAddr, fiber.ListenConfig{
		DisableStartupMessage: true,
		ListenerNetwork:       fiber.NetworkTCP,
	}))
}

func serve(c fiber.Ctx) error {
	if m := c.Method(); m != fiber.MethodGet && m != fiber.MethodHead {
		c.Set("Allow", "GET, HEAD")
		c.Set("Cache-Control", "no-store")
		return c.SendStatus(fiber.StatusMethodNotAllowed)
	}

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

	h.Set("Accept-Ranges", "bytes")
	if rng := c.Get("Range"); rng != "" && c.Method() == fiber.MethodGet {
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

func securityHeaders(c fiber.Ctx) {
	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	c.Set("X-Frame-Options", "DENY")
	c.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	c.Set("Content-Security-Policy", contentSecurityPolicy)
	c.Set("Permissions-Policy", permissionsPolicy)
}

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

func redirectTarget(dir string) string {
	return "/" + strings.TrimLeft((&url.URL{Path: dir}).EscapedPath(), "/")
}

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
	rangeIgnore = iota
	rangeOK
	rangeUnsatisfiable
)

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

	if first == "" {
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
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
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
	if !compressible(ctype) {
		cc += ", no-transform"
	}

	h := fnv.New64a()
	_, _ = h.Write(body)
	etag := fmt.Sprintf(`"%016x"`, h.Sum64())

	files[rel] = &file{ctype: ctype, cc: cc, etag: etag, body: body}
}

func compressible(ctype string) bool {
	return strings.HasPrefix(ctype, "text/") ||
		strings.Contains(ctype, "javascript") ||
		strings.Contains(ctype, "json") ||
		strings.Contains(ctype, "svg") ||
		strings.Contains(ctype, "xml") ||
		strings.Contains(ctype, "x-icon") ||
		strings.Contains(ctype, "font/ttf") ||
		strings.Contains(ctype, "font/otf") ||
		strings.Contains(ctype, "application/wasm")
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

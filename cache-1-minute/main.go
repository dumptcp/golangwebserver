package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gofiber/fiber/v3"
	"github.com/klauspost/compress/zstd"
)

var (
	siteRoot   = envStr("ROOT", "/root/www")
	listenAddr = ":" + envStr("PORT", "80")
)

const (
	htmlCacheControl  = "public, max-age=60, no-transform"
	assetCacheControl = "public, max-age=31536000, immutable, no-transform"

	zstdLevel     = zstd.SpeedDefault
	brotliQuality = brotli.DefaultCompression
	gzipLevel     = gzip.DefaultCompression

	contentSecurityPolicy = "default-src 'self'; " +
		"base-uri 'self'; " +
		"form-action 'self'; " +
		"frame-ancestors 'none'; " +
		"img-src 'self' data:; " +
		"style-src 'self' 'unsafe-inline'; " +
		"script-src 'self' 'unsafe-inline'; " +
		"font-src 'self'; " +
		"connect-src 'self'; " +
		"object-src 'none'; " +
		"upgrade-insecure-requests"

	permissionsPolicy = "accelerometer=(), autoplay=(), camera=(), " +
		"display-capture=(), encrypted-media=(), fullscreen=(), " +
		"geolocation=(), gyroscope=(), magnetometer=(), microphone=(), " +
		"midi=(), payment=(), picture-in-picture=(), publickey-credentials-get=(), " +
		"screen-wake-lock=(), sync-xhr=(), usb=(), xr-spatial-tracking=()"
)

type file struct {
	ctype, cc, etag  string
	body, br, gz, zs []byte
}

var files = map[string]*file{}

func main() {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstdLevel))
	if err != nil {
		log.Fatalf("zstd init: %v", err)
	}
	load(siteRoot, enc)
	_ = enc.Close()

	if len(files) == 0 {
		log.Fatalf("no files found under %s", siteRoot)
	}
	log.Printf("loaded %d files from %s", len(files), siteRoot)

	app := fiber.New(fiber.Config{
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  960 * time.Second, // must outlast Cloudflare's 900s connection reuse, or it causes 520s
	})
	app.All("/*", serve)
	log.Printf("listening on %s", listenAddr)
	log.Fatal(app.Listen(listenAddr, fiber.ListenConfig{DisableStartupMessage: true}))
}

func serve(c fiber.Ctx) error {
	// Fiber gives the raw path, so "/my%20file.html" would never match a file
	// named "my file.html". Decode it first; keep the raw path if it is malformed.
	p := c.Path()
	if u, err := url.PathUnescape(p); err == nil {
		p = u
	}

	if strings.HasSuffix(p, "/index.html") {
		c.Set("Location", strings.TrimSuffix(p, "index.html"))
		c.Set("Cache-Control", assetCacheControl)
		return c.SendStatus(fiber.StatusMovedPermanently)
	}

	key := strings.TrimPrefix(p, "/")
	if key == "" || strings.HasSuffix(key, "/") {
		key += "index.html"
	}

	f := files[key]
	if f == nil {
		c.Set("Cache-Control", "no-store")
		return c.SendStatus(fiber.StatusNotFound)
	}

	if inm := c.Get("If-None-Match"); inm != "" && strings.Contains(inm, f.etag) {
		c.Set("ETag", f.etag)
		c.Set("Cache-Control", f.cc)
		c.Set("Vary", "Accept-Encoding")
		return c.SendStatus(fiber.StatusNotModified)
	}

	body, enc := f.body, ""
	ae := c.Get("Accept-Encoding")
	switch {
	case f.zs != nil && strings.Contains(ae, "zstd"):
		body, enc = f.zs, "zstd"
	case f.br != nil && strings.Contains(ae, "br"):
		body, enc = f.br, "br"
	case f.gz != nil && strings.Contains(ae, "gzip"):
		body, enc = f.gz, "gzip"
	}

	h := &c.Response().Header
	h.Set("ETag", f.etag)
	h.Set("Cache-Control", f.cc)
	h.Set("Vary", "Accept-Encoding")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("Permissions-Policy", permissionsPolicy)

	h.SetContentType(f.ctype)
	if enc != "" {
		h.Set("Content-Encoding", enc)
	}
	return c.Send(body)
}

func load(root string, enc *zstd.Encoder) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
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
		add(filepath.ToSlash(rel), body, "", enc)
		return nil
	})
}

func add(rel string, body []byte, ctype string, enc *zstd.Encoder) {
	ext := strings.ToLower(filepath.Ext(rel))
	cc := assetCacheControl
	if ctype == "" {
		ctype = mime.TypeByExtension(ext)
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		switch ext {
		case ".html", ".htm":
			ctype = "text/html; charset=utf-8"
		case ".css":
			ctype = "text/css; charset=utf-8"
		case ".js", ".mjs":
			ctype = "text/javascript; charset=utf-8"
		case ".json", ".webmanifest":
			ctype = "application/json; charset=utf-8"
		case ".svg":
			ctype = "image/svg+xml; charset=utf-8"
		case ".txt":
			ctype = "text/plain; charset=utf-8"
		case ".xml":
			ctype = "application/xml; charset=utf-8"
		case ".ico":
			ctype = "image/x-icon"
		case ".woff":
			ctype = "font/woff"
		case ".woff2":
			ctype = "font/woff2"
		case ".ttf":
			ctype = "font/ttf"
		case ".otf":
			ctype = "font/otf"
		case ".webp":
			ctype = "image/webp"
		case ".avif":
			ctype = "image/avif"
		case ".wasm":
			ctype = "application/wasm"
		}
	}
	if ext == ".html" || ext == ".htm" {
		cc = htmlCacheControl
	}

	h := fnv.New64a()
	_, _ = h.Write(body)
	etag := fmt.Sprintf(`"%016x"`, h.Sum64())

	f := &file{ctype: ctype, cc: cc, etag: etag, body: body}
	if compressible(ctype) {
		if zs := enc.EncodeAll(body, make([]byte, 0, len(body)/2)); len(zs) > 0 && len(zs) < len(body) {
			f.zs = zs
		}
		if br := brotliOf(body); len(br) > 0 && len(br) < len(body) {
			f.br = br
		}
		if gz := gzipOf(body); len(gz) > 0 && len(gz) < len(body) {
			f.gz = gz
		}
	}
	files[rel] = f
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
		strings.Contains(ctype, "font/woff")
}

func brotliOf(b []byte) []byte {
	buf := bytes.NewBuffer(make([]byte, 0, len(b)/2))
	w := brotli.NewWriterOptions(buf, brotli.WriterOptions{Quality: brotliQuality, LGWin: 24})
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

func gzipOf(b []byte) []byte {
	buf := bytes.NewBuffer(make([]byte, 0, len(b)/2))
	w, err := gzip.NewWriterLevel(buf, gzipLevel)
	if err != nil {
		return nil
	}
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

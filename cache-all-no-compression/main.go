package main

import (
	"fmt"
	"hash/fnv"
	"io/fs"
	"log"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
)

var (
	siteRoot   = envStr("ROOT", "/root/www")
	listenAddr = ":" + envStr("PORT", "80")
)

const (
	assetCacheControl = "public, max-age=31536000, immutable, no-transform"

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
	ctype, cc, etag string
	body            []byte
}

var files = map[string]*file{}

func main() {
	load(siteRoot)

	if len(files) == 0 {
		log.Fatalf("no files found under %s", siteRoot)
	}
	log.Printf("loaded %d files from %s", len(files), siteRoot)

	app := fiber.New(fiber.Config{
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  75 * time.Second,
	})
	app.All("/*", serve)
	log.Printf("listening on %s", listenAddr)
	log.Fatal(app.Listen(listenAddr, fiber.ListenConfig{DisableStartupMessage: true}))
}

func serve(c fiber.Ctx) error {
	p := c.Path()

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
		return c.SendStatus(fiber.StatusNotModified)
	}

	h := &c.Response().Header
	h.Set("ETag", f.etag)
	h.Set("Cache-Control", f.cc)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("Permissions-Policy", permissionsPolicy)

	h.SetContentType(f.ctype)
	return c.Send(f.body)
}

func load(root string) {
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
		add(filepath.ToSlash(rel), body, "")
		return nil
	})
}

func add(rel string, body []byte, ctype string) {
	ext := strings.ToLower(filepath.Ext(rel))
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

	h := fnv.New64a()
	_, _ = h.Write(body)
	etag := fmt.Sprintf(`"%016x"`, h.Sum64())

	files[rel] = &file{ctype: ctype, cc: assetCacheControl, etag: etag, body: body}
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

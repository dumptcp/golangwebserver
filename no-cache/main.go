package main

import (
	"bytes"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log"
	"math"
	"mime"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/andybalholm/brotli"
	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
)

// ================================================================ settings
// Version: no-cache (nothing cached, no compression).
// This block is the only difference between the eight versions of this server; it
// matches what the Rust version in the same folder of dumptcp/rustwebserver sends.

// Keep a Brotli copy (strongest level) of every text file and send it to requests that
// accept Brotli. Cloudflare always asks for Brotli and decompresses it for visitors
// that cannot read it.
const compression = false

// Send an ETag with every file and answer 304 Not Modified while it still matches.
const etags = false

// Cache-Control for .html / .htm files, and for every other file.
const htmlCacheControl = "no-store, no-cache, must-revalidate, max-age=0"
const assetCacheControl = "no-store, no-cache, must-revalidate, max-age=0"

// Add `no-transform` only to file types that are already compressed (images, fonts,
// video, ...), so Cloudflare can still compress text files itself.
const noTransformOnlyForPrecompressedTypes = true

// Cache-Control for redirects, and for 404 / 405 / 416 responses.
const redirectCacheControl = "no-store, no-cache, must-revalidate, max-age=0"
const errorCacheControl = "no-store, no-cache, must-revalidate, max-age=0"

// ================================================================ server
var (
	siteRoot   = envStr("ROOT", "/root/www")
	listenAddr = net.JoinHostPort(envStr("HOST", ""), envStr("PORT", "80"))
)

const (
	brotliFast, brotliBest           = 6, 11
	brotliMinWindow, brotliMaxWindow = 16, 22

	startupGCPercent = 25

	readBufferSize  = 128 * 1024        // also the largest request head; Cloudflare forwards at least 64 KB
	readTimeout     = 60 * time.Second  // to receive a whole request
	idleTimeout     = 960 * time.Second // between requests on a kept-alive connection
	writeTimeout    = time.Hour         // big downloads to slow visitors
	shutdownTimeout = 30 * time.Second  // Ctrl+C: finish in-flight requests first
	maxConnections  = 10_000_000

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

// Header names and values, made once so a request only copies bytes.
var (
	hAcceptRanges    = []byte("Accept-Ranges")
	hAllow           = []byte("Allow")
	hCacheControl    = []byte("Cache-Control")
	hContentRange    = []byte("Content-Range")
	hETag            = []byte("ETag")
	hLocation        = []byte("Location")
	hVary            = []byte("Vary")
	hNosniff         = []byte("X-Content-Type-Options")
	hReferrer        = []byte("Referrer-Policy")
	hFrame           = []byte("X-Frame-Options")
	hHSTS            = []byte("Strict-Transport-Security")
	hCSP             = []byte("Content-Security-Policy")
	hPermissions     = []byte("Permissions-Policy")
	vBytes           = []byte("bytes")
	vAcceptEncoding  = []byte("Accept-Encoding")
	vGetHead         = []byte("GET, HEAD")
	vNosniff         = []byte("nosniff")
	vReferrer        = []byte("strict-origin-when-cross-origin")
	vDeny            = []byte("DENY")
	vHSTS            = []byte("max-age=31536000; includeSubDomains")
	vCSP             = []byte(contentSecurityPolicy)
	vPermissions     = []byte(permissionsPolicy)
	vBr              = []byte("br")
	vRedirectCC      = []byte(redirectCacheControl)
	vErrorCC         = []byte(errorCacheControl)
	vTextPlain       = []byte("text/plain; charset=utf-8")
	msgRangeNotSatis = []byte("Range Not Satisfiable")
)

type variant struct {
	body []byte
	enc  []byte // nil = sent as-is
}

// [original, Brotli]: Cloudflare always asks for `br, gzip` and decompresses Brotli
// itself for clients without it, so no gzip copy is kept.
type pickTable [2]variant

const acceptBr = 1

type file struct {
	ctype, cc, etag []byte
	body            []byte

	pick atomic.Pointer[pickTable]

	ranges bool
}

// files maps a path (without the leading "/") to its file. A folder's index.html is
// also found under "folder/" (and the top one under ""), so no request builds a key.
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
	signal.Ignore(syscall.SIGPIPE)

	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err == nil {
		log.Printf("open file limit: %d", lim.Cur) // Go raises it to the hard limit itself
	}

	normalGCPercent := debug.SetGCPercent(startupGCPercent)

	load(siteRoot)
	if len(files) == 0 {
		log.Fatalf("no files found under %s", siteRoot)
	}
	log.Printf("loaded %d files from %s", countFiles(), siteRoot)
	go func() {
		if compression {
			if procs := runtime.GOMAXPROCS(0); procs < 2 {
				runtime.GOMAXPROCS(2)
				defer runtime.GOMAXPROCS(procs)
			}
			recompress()
		}
		debug.SetGCPercent(normalGCPercent)
		debug.FreeOSMemory()
	}()

	app := fiber.New(fiber.Config{
		ReadTimeout:    readTimeout,
		WriteTimeout:   writeTimeout,
		IdleTimeout:    idleTimeout,
		ReadBufferSize: readBufferSize,
		// Hand the read buffer back while a connection waits for its next request, so
		// thousands of open Cloudflare connections don't each keep one.
		ReduceMemoryUsage: true,
		Concurrency:       maxConnections,
		// The handler reads the path as sent; these skip a lowercased copy of it.
		CaseSensitive: true,
		StrictRouting: true,
		ErrorHandler:  onError,
	})
	app.All("/*", serve)

	done := make(chan struct{})
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		<-stop
		log.Printf("shutting down: finishing in-flight requests (up to %s)", shutdownTimeout)
		_ = app.ShutdownWithTimeout(shutdownTimeout)
		close(done)
	}()

	log.Printf("listening on %s", listenAddr)
	if err := app.Listen(listenAddr, fiber.ListenConfig{
		DisableStartupMessage: true,
		ListenerNetwork:       fiber.NetworkTCP,
	}); err != nil {
		log.Fatal(err)
	}
	<-done
}

func serve(c fiber.Ctx) error {
	ctx := c.RequestCtx()
	req, h := &ctx.Request.Header, &ctx.Response.Header
	isGet := req.IsGet()
	if !isGet && !req.IsHead() {
		h.SetCanonical(hAllow, vGetHead)
		plain(ctx, fiber.StatusMethodNotAllowed, vErrorCC)
		return nil
	}

	// The path exactly as sent (Fiber 3.6+ resolves "." and ".." in c.Path()), decoded once.
	raw := ctx.URI().PathOriginal()
	p := decodePath(unsafe.String(unsafe.SliceData(raw), len(raw)))

	if strings.HasSuffix(p, "/index.html") {
		redirect(ctx, p[:len(p)-len("index.html")])
		return nil
	}

	key := strings.TrimPrefix(p, "/")
	f := files[key]
	if f == nil {
		if !strings.HasSuffix(p, "/") && files[key+"/"] != nil {
			redirect(ctx, p+"/")
			return nil
		}
		notFound(ctx)
		return nil
	}

	if etags {
		if inm := req.Peek(fiber.HeaderIfNoneMatch); len(inm) > 0 && bytes.Contains(inm, f.etag) {
			h.SetStatusCode(fiber.StatusNotModified)
			h.SetCanonical(hETag, f.etag)
			h.SetCanonical(hCacheControl, f.cc)
			if compression {
				h.SetCanonical(hVary, vAcceptEncoding)
			}
			return nil
		}
	}

	v := &f.pick.Load()[acceptMask(req)]
	body := v.body

	if f.ranges {
		h.SetCanonical(hAcceptRanges, vBytes)
		if rng := req.Peek(fiber.HeaderRange); len(rng) > 0 && isGet {
			if ir := req.Peek(fiber.HeaderIfRange); len(ir) == 0 || (etags && bytes.Equal(ir, f.etag)) {
				switch start, end, res := parseRange(rng, len(body)); res {
				case rangeOK:
					h.SetCanonical(hContentRange, fmt.Appendf(nil, "bytes %d-%d/%d", start, end, len(body)))
					h.SetStatusCode(fiber.StatusPartialContent)
					body = body[start : end+1]
				case rangeUnsatisfiable:
					h.SetCanonical(hContentRange, fmt.Appendf(nil, "bytes */%d", len(body)))
					h.SetStatusMessage(msgRangeNotSatis)
					plain(ctx, fiber.StatusRequestedRangeNotSatisfiable, vErrorCC)
					return nil
				}
			}
		}
	}

	if etags {
		h.SetCanonical(hETag, f.etag)
	}
	h.SetCanonical(hCacheControl, f.cc)
	if compression {
		h.SetCanonical(hVary, vAcceptEncoding)
	}
	securityHeaders(h)
	h.SetContentTypeBytes(f.ctype)
	if v.enc != nil {
		h.SetContentEncodingBytes(v.enc)
	}
	ctx.Response.SetBodyRaw(body)
	return nil
}

// acceptMask reports whether the request accepts Brotli, per RFC 9110 §12.5.3: codings
// are case-insensitive, `;q=0` refuses one, and `*` stands for every coding not named.
func acceptMask(req *fasthttp.RequestHeader) int {
	ae := req.Peek(fiber.HeaderAcceptEncoding)
	for _, b := range ae {
		if (b < ' ' && b != '\t') || b > '~' {
			return 0 // not a valid header value: send the file as-is
		}
	}
	var br, any int8 // 0 = not named, 1 = accepted, -1 = refused
	for len(ae) > 0 {
		var item []byte
		item, ae, _ = bytes.Cut(ae, []byte{','})
		coding, params, _ := bytes.Cut(item, []byte{';'})
		coding = bytes.TrimSpace(coding)
		var slot *int8
		switch {
		case len(coding) == 2 && coding[0]|0x20 == 'b' && coding[1]|0x20 == 'r':
			slot = &br
		case len(coding) == 1 && coding[0] == '*':
			slot = &any
		default:
			continue
		}
		*slot = 1
		for len(params) > 0 {
			var p []byte
			p, params, _ = bytes.Cut(params, []byte{';'})
			p = bytes.TrimSpace(p)
			if len(p) >= 2 && p[0]|0x20 == 'q' && p[1] == '=' {
				if q, err := strconv.ParseFloat(string(bytes.TrimSpace(p[2:])), 32); err == nil && q <= 0 {
					*slot = -1
				}
			}
		}
	}
	if br == 1 || (br == 0 && any == 1) {
		return acceptBr
	}
	return 0
}

func securityHeaders(h *fasthttp.ResponseHeader) {
	h.SetCanonical(hNosniff, vNosniff)
	h.SetCanonical(hReferrer, vReferrer)
	h.SetCanonical(hFrame, vDeny)
	h.SetCanonical(hHSTS, vHSTS)
	h.SetCanonical(hCSP, vCSP)
	h.SetCanonical(hPermissions, vPermissions)
}

// plain answers with a status code, its name as a short text body and Cache-Control.
func plain(ctx *fasthttp.RequestCtx, code int, cc []byte) {
	h := &ctx.Response.Header
	h.SetStatusCode(code)
	if cc != nil {
		h.SetCanonical(hCacheControl, cc)
	}
	h.SetContentTypeBytes(vTextPlain)
	if msg := h.StatusMessage(); len(msg) > 0 {
		ctx.Response.SetBody(msg)
	} else {
		ctx.Response.SetBodyString(fasthttp.StatusMessage(code))
	}
}

// onError answers requests that never reach serve: unknown methods get
// 501 Not Implemented, unreadable requests an empty 400 (or the status fasthttp chose).
func onError(c fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	var fe *fiber.Error
	if errors.As(err, &fe) {
		code = fe.Code
	}
	ctx := c.RequestCtx()
	ctx.Response.Reset()
	if code == fiber.StatusNotImplemented {
		plain(ctx, code, nil)
		return nil
	}
	ctx.Response.Header.SetStatusCode(code)
	ctx.Response.Header.SetNoDefaultContentType(true)
	return nil
}

func notFound(ctx *fasthttp.RequestCtx) {
	f := files["404.html"]
	if f == nil {
		plain(ctx, fiber.StatusNotFound, vErrorCC)
		return
	}
	v := &f.pick.Load()[acceptMask(&ctx.Request.Header)]
	h := &ctx.Response.Header
	h.SetStatusCode(fiber.StatusNotFound)
	h.SetCanonical(hCacheControl, vErrorCC)
	if compression {
		h.SetCanonical(hVary, vAcceptEncoding)
	}
	securityHeaders(h)
	h.SetContentTypeBytes(f.ctype)
	if v.enc != nil {
		h.SetContentEncodingBytes(v.enc)
	}
	ctx.Response.SetBodyRaw(v.body)
}

func redirect(ctx *fasthttp.RequestCtx, dir string) {
	loc := redirectTarget(dir)
	if q := ctx.URI().QueryString(); len(q) > 0 && visibleASCII(q) {
		loc = append(append(loc, '?'), q...)
	}
	ctx.Response.Header.SetCanonical(hLocation, loc)
	plain(ctx, fiber.StatusMovedPermanently, vRedirectCC)
}

func visibleASCII(b []byte) bool {
	for _, c := range b {
		if c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

// redirectTarget percent-encodes a path for Location, keeping letters, digits and
// / - _ . ~ $ & + , : ; = @, the same as the Rust version.
func redirectTarget(dir string) []byte {
	const hex = "0123456789ABCDEF"
	out := make([]byte, 0, len(dir)+8)
	out = append(out, '/')
	dir = strings.TrimLeft(dir, "/")
	for i := 0; i < len(dir); i++ {
		c := dir[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("/-_.~$&+,:;=@", c) >= 0 {
			out = append(out, c)
		} else {
			out = append(out, '%', hex[c>>4], hex[c&15])
		}
	}
	return out
}

// decodePath turns %XX sequences into bytes; anything else is left as it is.
func decodePath(p string) string {
	if strings.IndexByte(p, '%') < 0 {
		return p
	}
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		if p[i] == '%' && i+2 < len(p) && unhex(p[i+1]) >= 0 && unhex(p[i+2]) >= 0 {
			out = append(out, byte(unhex(p[i+1])<<4|unhex(p[i+2])))
			i += 2
			continue
		}
		out = append(out, p[i])
	}
	return strings.ToValidUTF8(string(out), "�")
}

func unhex(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c - 'a' + 10)
	case 'A' <= c && c <= 'F':
		return int(c - 'A' + 10)
	}
	return -1
}

const (
	rangeIgnore = iota
	rangeOK
	rangeUnsatisfiable
)

func parseRange(header []byte, size int) (start, end, result int) {
	const unit = "bytes="
	if size == 0 || len(header) < len(unit) || !strings.EqualFold(string(header[:len(unit)]), unit) {
		return 0, 0, rangeIgnore
	}
	spec := strings.TrimSpace(string(header[len(unit):]))
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
	for rel, f := range files {
		if rel == "index.html" {
			files[""] = f
		} else if dir, ok := strings.CutSuffix(rel, "/index.html"); ok {
			files[dir+"/"] = f
		}
	}
}

func countFiles() int {
	n := 0
	for rel := range files {
		if rel != "" && !strings.HasSuffix(rel, "/") {
			n++
		}
	}
	return n
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
	if noTransformOnlyForPrecompressedTypes && !compressible(ctype) {
		cc += ", no-transform"
	}

	h := fnv.New64a()
	_, _ = h.Write(body)
	etag := fmt.Sprintf(`"%016x"`, h.Sum64())

	f := &file{ctype: []byte(ctype), cc: []byte(cc), etag: []byte(etag), body: body}
	var br []byte
	if compression && compressible(ctype) {
		br = smaller(brotliOf(body, brotliFast, brotliMaxWindow), body)
	}
	f.pick.Store(newPickTable(body, br))
	f.ranges = br == nil
	files[rel] = f
}

func recompress() {
	workers := max(1, runtime.GOMAXPROCS(0)/2)
	start := time.Now()
	work := make(chan *file)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range work {
				old := f.pick.Load()[acceptBr]
				br := smaller(brotliOf(f.body, brotliBest, brotliWindow(len(f.body))), old.body)
				if br == nil && old.enc != nil {
					br = old.body
				}
				f.pick.Store(newPickTable(f.body, br))
			}
		}()
	}
	n := 0
	seen := map[*file]bool{}
	for _, f := range files {
		if !f.ranges && !seen[f] {
			seen[f] = true
			work <- f
			n++
		}
	}
	close(work)
	wg.Wait()
	log.Printf("recompressed %d files at the strongest levels in %s", n, time.Since(start).Round(time.Millisecond))
}

func smaller(compressed, than []byte) []byte {
	if len(compressed) > 0 && len(compressed) < len(than) {
		return bytes.Clone(compressed)
	}
	return nil
}

func newPickTable(body, br []byte) *pickTable {
	t := pickTable{{body: body}, {body: body}}
	if br != nil && len(br) < len(body) {
		t[acceptBr] = variant{body: br, enc: vBr}
	}
	return &t
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

func brotliOf(b []byte, quality, lgwin int) []byte {
	buf := bytes.NewBuffer(make([]byte, 0, len(b)/2))
	w := brotli.NewWriterOptions(buf, brotli.WriterOptions{Quality: quality, LGWin: lgwin})
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

func brotliWindow(size int) int {
	lgwin := brotliMinWindow
	for lgwin < brotliMaxWindow && 1<<lgwin-16 < size {
		lgwin++
	}
	return lgwin
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
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
// Version: no-cache+compression (nothing cached, Brotli compression).
// This block is the only difference between the eight versions of this server; it
// matches what the Rust version in the same folder of dumptcp/rustwebserver sends.

// Keep a Brotli copy (strongest level) of every text file and send it to requests that
// accept Brotli. Cloudflare always asks for Brotli and decompresses it for visitors
// that cannot read it.
const compression = true

// Send an ETag with every file and answer 304 Not Modified while it still matches.
const etags = false

// Cache-Control for .html / .htm files, and for every other file.
const htmlCacheControl = "no-store, no-cache, must-revalidate, max-age=0, no-transform"
const assetCacheControl = "no-store, no-cache, must-revalidate, max-age=0, no-transform"

// Add `no-transform` only to file types that are already compressed (images, fonts,
// video, ...), so Cloudflare can still compress text files itself.
const noTransformOnlyForPrecompressedTypes = false

// Cache-Control for redirects, and for 404 / 405 / 416 responses.
const redirectCacheControl = "no-store, no-cache, must-revalidate, max-age=0, no-transform"
const errorCacheControl = "no-store, no-cache, must-revalidate, max-age=0, no-transform"

// ================================================================ server
var (
	siteRoot   = envStr("ROOT", "/root/www")
	listenAddr = net.JoinHostPort(envStr("HOST", ""), envStr("PORT", "80"))
)

const (
	brotliFast, brotliBest           = 6, 11
	brotliMinWindow, brotliMaxWindow = 16, 22

	startupGCPercent = 25

	defaultMaxInMemory = 2 * 1024 * 1024

	// Unsent bytes allowed in the kernel send queue of one socket.
	tcpNotSentLowat = 16 * 1024
	// Used when physical memory and the cgroup limit cannot be read.
	fallbackUsableMemory = 1 << 30

	// Per-connection ceilings. readBufferSize is also the largest request head
	// fasthttp accepts, so an incomplete header stops at 16 KB. The body stops
	// at bodyLimit. Worst case while one request is in flight is that 16 KB
	// read buffer, a 16 KB body and the 4 KB write buffer. ReduceMemoryUsage
	// drops the read and body buffers between requests. A 128 KB head buffer
	// pooled per connection is what let RSS climb into gigabytes on a site of
	// a few dozen kilobytes.
	readBufferSize  = 16 * 1024
	writeBufferSize = 4 * 1024
	bodyLimit       = 16 * 1024

	// Seconds an idle keep-alive connection stays open. 120 keeps a
	// Cloudflare-fronted flood from pinning hundreds of thousands of sockets.
	// At 910 about 300k stayed open, and the mass close left an orphan-socket
	// storm that killed SSH. KEEP_ALIVE overrides this (minimum 1).
	defaultKeepAlive = 120

	readTimeout     = 10 * time.Second // to receive a whole request
	writeTimeout    = 30 * time.Second // slow or stalled readers are dropped here
	shutdownTimeout = 30 * time.Second // Ctrl+C: finish in-flight requests first

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
		"upgrade-insecure-requests; " +
		"require-trusted-types-for 'script'; trusted-types 'none'"

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
	hCOOP            = []byte("Cross-Origin-Opener-Policy")
	hCORP            = []byte("Cross-Origin-Resource-Policy")
	hPermissions     = []byte("Permissions-Policy")
	vBytes           = []byte("bytes")
	vAcceptEncoding  = []byte("Accept-Encoding")
	vGetHead         = []byte("GET, HEAD")
	vNosniff         = []byte("nosniff")
	vReferrer        = []byte("strict-origin-when-cross-origin")
	vDeny            = []byte("DENY")
	vHSTS            = []byte("max-age=31536000; includeSubDomains; preload")
	vCSP             = []byte(contentSecurityPolicy)
	vCOOP            = []byte("same-origin")
	vCORP            = []byte("same-origin")
	vPermissions     = []byte(permissionsPolicy)
	vBr              = []byte("br")
	vHTTP10          = []byte("HTTP/1.0")
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
	body            []byte // nil if the file is served from disk
	path            string
	size            int

	pick atomic.Pointer[pickTable]

	ranges bool
}

var (
	maxInMemory = defaultMaxInMemory
	// 0 means no connection cap. MAX_CONNECTIONS, when set above 0, is the cap.
	maxConnections = 0
	// cacheBudget is the total RAM for file bodies and their pre-compressed
	// copies. CACHE_MEMORY overrides it; the default is usable memory / 4.
	cacheBudget uint64
	cachedRAM   uint64
	cacheMu     sync.Mutex
)

// overloadResponse is the whole answer past MAX_CONNECTIONS.
var overloadResponse = []byte("HTTP/1.1 503 Service Unavailable\r\nConnection: close\r\nRetry-After: 1\r\nContent-Length: 0\r\n\r\n")

// How long a rejected connection may wait for the request bytes already on
// their way. Closing with those bytes still unread makes the kernel abort
// the socket, and the client never sees the 503. The wait runs off the
// accept loop.
const overloadDrain = 20 * time.Millisecond

// fileStream closes the underlying file after fasthttp finishes sending it.
type fileStream struct {
	r io.Reader
	c io.Closer
}

func (f fileStream) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f fileStream) Close() error               { return f.c.Close() }

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
	".yaml":        "text/x-yaml; charset=utf-8",
	".yml":         "text/x-yaml; charset=utf-8",
	".toml":        "text/x-toml; charset=utf-8",
	".ini":         "text/plain; charset=utf-8",
	".conf":        "text/plain; charset=utf-8",
	".cfg":         "text/plain; charset=utf-8",
	".log":         "text/plain; charset=utf-8",
	".xslt":        "text/xml; charset=utf-8",
	".dtd":         "text/xml; charset=utf-8",
	".vcf":         "text/x-vcard; charset=utf-8",
	".wasm":        "application/wasm",
	".pdf":         "application/pdf",
	".zip":         "application/zip",
	".tgz":         "application/x-compressed",
	".rar":         "application/x-rar-compressed",

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
	".avi":  "video/x-msvideo",
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
	".mid":  "audio/mid",
	".midi": "audio/mid",
	".aif":  "audio/aiff",
	".aiff": "audio/aiff",
}

func main() {
	ignoreSIGPIPE()

	maxInMemory = int(envUint("MAX_IN_MEMORY", defaultMaxInMemory))
	usable := usableMemory()
	// Raise the soft open-file limit to the hard limit. This does not cap
	// connections; the kernel still refuses a new socket at EMFILE.
	raiseOpenFileLimit()
	maxConnections = connectionCap()

	keepAlive := envUint("KEEP_ALIVE", defaultKeepAlive)
	if keepAlive < 1 {
		log.Fatalf("bad KEEP_ALIVE: must be at least 1")
	}
	// A value that does not fit in a time.Duration would wrap negative.
	if keepAlive > uint64(math.MaxInt64/int64(time.Second)) {
		log.Fatalf("bad KEEP_ALIVE: %d", keepAlive)
	}
	idleTimeout := time.Duration(keepAlive) * time.Second

	memLimit := applyMemoryLimit(usable)
	cacheBudget = envBytes("CACHE_MEMORY", usable/4)
	log.Printf("usable memory: %d bytes, max connections: %s, memory limit: %d bytes, cache budget: %d bytes, keep-alive: %d seconds", usable, connectionCapLabel(maxConnections), memLimit, cacheBudget, keepAlive)

	normalGCPercent := debug.SetGCPercent(startupGCPercent)

	// Bind before loading files, so a port that is privileged or already
	// taken fails before a large site is read.
	// Listen reads /proc/sys/net/core/somaxconn and passes that value to
	// listen(2), so the accept backlog is already the kernel maximum.
	// On Linux, TCP_NOTSENT_LOWAT and TCP_USER_TIMEOUT are set on the
	// listening socket before bind. Linux copies both onto accepted sockets
	// (inet_csk_clone_lock does not clear them).
	ln, err := listen(listenAddr)
	if err != nil {
		fatalListen(err)
	}
	log.Printf("listening on %s", listenAddr)

	load(siteRoot)
	if len(files) == 0 {
		log.Fatalf("no files found under %s", siteRoot)
	}
	mem, disk, memBytes, diskBytes := countLoaded()
	log.Printf("loaded %d files from %s (%d in memory / %d bytes, %d served from disk / %d bytes), cache budget %d bytes", mem+disk, siteRoot, mem, memBytes, disk, diskBytes, cacheBudget)
	log.Printf("max in-memory file: %d bytes, max connections: %s", maxInMemory, connectionCapLabel(maxConnections))
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

	// fasthttp answers 503 on its own once Concurrency workers are busy, and
	// that response is not the one this server sends. Concurrency is
	// math.MaxInt32 so the worker pool does not turn connections away.
	// Workers are created on demand, so the ceiling does not allocate them
	// up front. When MAX_CONNECTIONS is set above 0, the listener below
	// enforces that cap and writes the exact 503.
	// SleepWhenConcurrencyLimitsExceeded stays 0: the accept loop never waits.
	app := fiber.New(fiber.Config{
		ReadTimeout:     readTimeout,
		WriteTimeout:    writeTimeout,
		IdleTimeout:     idleTimeout,
		ReadBufferSize:  readBufferSize,
		WriteBufferSize: writeBufferSize,
		BodyLimit:       bodyLimit,
		// This server never reads a form. Skip the parse so a multipart
		// upload is not allocated before the 405 is sent.
		DisablePreParseMultipartForm: true,
		// Drop read and body buffers between requests. Idle keep-alive
		// connections would otherwise retain them until the connection
		// closes. The cost is more allocations on a busy connection.
		ReduceMemoryUsage: true,
		Concurrency:       math.MaxInt32,
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

	served := net.Listener(ln)
	var gate *connGate
	if maxConnections > 0 {
		gate = &connGate{max: int64(maxConnections)}
		served = &capListener{Listener: ln, gate: gate}
	}
	if err := app.Listener(served, fiber.ListenConfig{
		DisableStartupMessage: true,
		BeforeServeFunc: func(app *fiber.App) error {
			if gate == nil {
				return nil
			}
			prev := app.Server().ConnState
			app.Server().ConnState = func(c net.Conn, state fasthttp.ConnState) {
				if state == fasthttp.StateClosed || state == fasthttp.StateHijacked {
					gate.release()
				}
				if prev != nil {
					prev(c, state)
				}
			}
			return nil
		},
	}); err != nil {
		log.Fatal(err)
	}
	<-done
}

// connGate counts connections handed to the server. The cap is
// MAX_CONNECTIONS. Past it the listener answers 503 itself.
type connGate struct {
	open atomic.Int64
	max  int64
}

func (g *connGate) acquire() bool {
	for {
		cur := g.open.Load()
		if cur >= g.max {
			return false
		}
		if g.open.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

func (g *connGate) release() {
	g.open.Add(-1)
}

// capListener accepts every connection. Under the cap the raw connection is
// returned, so fasthttp can still use writev and sendfile. Over the cap the
// connection is answered with 503 on another goroutine and Accept goes back
// for the next one, so the accept loop does not stall and does not stop.
type capListener struct {
	net.Listener
	gate *connGate
}

func (l *capListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.gate.acquire() {
			return c, nil
		}
		// Answer off the accept loop so a slow client cannot stall it.
		go rejectOverload(c)
	}
}

// rejectOverload writes the 503 and closes. It first drops request bytes
// that arrive within overloadDrain, so close is a normal FIN and the
// response is what the client reads.
func rejectOverload(c net.Conn) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(overloadDrain))
	var buf [4096]byte
	for {
		if _, err := c.Read(buf[:]); err != nil {
			break
		}
	}
	_ = c.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	_, _ = c.Write(overloadResponse)
}

func fatalListen(err error) {
	// os.IsPermission does not unwrap a net.OpError from Listen.
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		log.Fatalf("cannot listen on %s: permission denied. Run as root, run `sudo setcap cap_net_bind_service=+ep %s`, or set PORT=8080 behind a reverse proxy.", listenAddr, os.Args[0])
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		log.Fatalf("cannot listen on %s: the port is in use", listenAddr)
	}
	log.Fatal(err)
}

func serve(c fiber.Ctx) error {
	ctx := c.RequestCtx()
	req, h := &ctx.Request.Header, &ctx.Response.Header
	if !req.IsHTTP11() {
		h.SetProtocol(vHTTP10) // HTTP/1.0 requests get an HTTP/1.0 answer, as in the Rust versions
	}
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
			if compression && f.body != nil {
				h.SetCanonical(hVary, vAcceptEncoding)
			}
			return nil
		}
	}

	if f.body == nil {
		return serveDisk(ctx, f, isGet)
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
	setBody(ctx, body)
	return nil
}

// acceptMask reports whether the request accepts Brotli, per RFC 9110 §12.5.3: codings
// are case-insensitive, `;q=0` refuses one, and `*` stands for every coding not named.

func serveDisk(ctx *fasthttp.RequestCtx, f *file, isGet bool) error {
	req, h := &ctx.Request.Header, &ctx.Response.Header
	start, end := 0, 0
	if f.size > 0 {
		end = f.size - 1
	}
	code := fiber.StatusOK
	if f.ranges {
		h.SetCanonical(hAcceptRanges, vBytes)
		if rng := req.Peek(fiber.HeaderRange); len(rng) > 0 && isGet {
			if ir := req.Peek(fiber.HeaderIfRange); len(ir) == 0 || (etags && bytes.Equal(ir, f.etag)) {
				switch s, e, res := parseRange(rng, f.size); res {
				case rangeOK:
					h.SetCanonical(hContentRange, fmt.Appendf(nil, "bytes %d-%d/%d", s, e, f.size))
					h.SetStatusCode(fiber.StatusPartialContent)
					start, end, code = s, e, fiber.StatusPartialContent
				case rangeUnsatisfiable:
					h.SetCanonical(hContentRange, fmt.Appendf(nil, "bytes */%d", f.size))
					h.SetStatusMessage(msgRangeNotSatis)
					plain(ctx, fiber.StatusRequestedRangeNotSatisfiable, vErrorCC)
					return nil
				}
			}
		}
	}
	if code == fiber.StatusOK {
		h.SetStatusCode(code)
	}
	if etags {
		h.SetCanonical(hETag, f.etag)
	}
	h.SetCanonical(hCacheControl, f.cc)
	securityHeaders(h)
	h.SetContentTypeBytes(f.ctype)
	n := 0
	if f.size > 0 {
		n = end - start + 1
	}
	if !isGet || n == 0 {
		h.SetContentLength(n)
		return nil
	}
	fh, err := os.Open(f.path)
	if err != nil {
		plain(ctx, fiber.StatusNotFound, vErrorCC)
		return nil
	}
	ctx.Response.SetBodyStream(fileStream{r: io.NewSectionReader(fh, int64(start), int64(n)), c: fh}, n)
	return nil
}

func acceptMask(req *fasthttp.RequestHeader) int {
	if !compression {
		return 0
	}
	ae := req.Peek(fiber.HeaderAcceptEncoding)
	// What Cloudflare sends on every request; skip parsing it.
	if string(ae) == "gzip, br" || string(ae) == "br, gzip" {
		return acceptBr
	}
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
	h.SetCanonical(hCOOP, vCOOP)
	h.SetCanonical(hCORP, vCORP)
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

// setBody hands the response its body. fasthttp leaves Content-Length out of a HEAD
// answer whose body is empty, so it is set here, as the Rust versions send it.
func setBody(ctx *fasthttp.RequestCtx, body []byte) {
	ctx.Response.SetBodyRaw(body)
	if len(body) == 0 {
		ctx.Response.Header.SetContentLength(0)
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
	if f.body == nil {
		_ = serveDisk(ctx, f, !ctx.IsHead())
		ctx.Response.Header.SetStatusCode(fiber.StatusNotFound)
		ctx.Response.Header.SetCanonical(hCacheControl, vErrorCC)
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
	setBody(ctx, v.body)
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
		// Dotfiles and dot-directories are not served, except the
		// .well-known directory and the files inside it. Symlinks are
		// skipped: WalkDir does not follow them, and a symlink is not a
		// regular file, so the link itself is not served either.
		if path != root && strings.HasPrefix(d.Name(), ".") {
			if d.Name() == ".well-known" && d.IsDir() {
				return nil
			}
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		// Larger than MAX_IN_MEMORY, or larger than the cache budget still
		// free: hash it in a small buffer and serve it from disk. A site
		// bigger than RAM is never read in whole.
		if info.Size() > int64(maxInMemory) || uint64(info.Size()) > cacheRemaining() {
			hash, size, err := hashFile(path)
			if err != nil {
				return nil
			}
			addDisk(rel, path, size, hash)
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		add(rel, path, body)
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

func countLoaded() (mem, disk int, memBytes, diskBytes uint64) {
	seen := map[*file]bool{}
	for rel, f := range files {
		if rel == "" || strings.HasSuffix(rel, "/") || seen[f] {
			continue
		}
		seen[f] = true
		if f.body == nil {
			disk++
			if f.size > 0 {
				diskBytes += uint64(f.size)
			}
			continue
		}
		mem++
		memBytes += uint64(len(f.body))
		if p := f.pick.Load(); p != nil && p[acceptBr].enc != nil {
			memBytes += uint64(len(p[acceptBr].body))
		}
	}
	return mem, disk, memBytes, diskBytes
}

func hashFile(path string) (uint64, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	h := fnv.New64a()
	buf := make([]byte, 64*1024)
	n := 0
	for {
		c, err := f.Read(buf)
		if c > 0 {
			_, _ = h.Write(buf[:c])
			n += c
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, err
		}
	}
	return h.Sum64(), n, nil
}

func add(rel, path string, body []byte) {
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
	sum := h.Sum64()

	var br []byte
	if compression && compressible(ctype) {
		br = smaller(brotliOf(body, brotliFast, brotliMaxWindow), body)
	}
	// The budget counts the file and its pre-compressed copy. If both do
	// not fit in what is left, the bytes are dropped and the file is served
	// from disk with the same headers, status and ranges.
	cost := uint64(len(body))
	if br != nil {
		cost += uint64(len(br))
	}
	if !reserveCache(cost) {
		addDisk(rel, path, len(body), sum)
		return
	}

	f := &file{ctype: []byte(ctype), cc: []byte(cc), etag: []byte(fmt.Sprintf(`"%016x"`, sum)), body: body}
	f.pick.Store(newPickTable(body, br))
	f.ranges = br == nil
	f.size = len(body)
	files[rel] = f
}

func addDisk(rel, path string, size int, hash uint64) {
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

	etag := fmt.Sprintf(`"%016x"`, hash)
	f := &file{ctype: []byte(ctype), cc: []byte(cc), etag: []byte(etag), path: path, size: size, ranges: true}
	f.pick.Store(newPickTable(nil, nil))
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
				oldExtra := 0
				if old.enc != nil {
					oldExtra = len(old.body)
				}
				br := smaller(brotliOf(f.body, brotliBest, brotliWindow(len(f.body))), old.body)
				if br == nil && old.enc != nil {
					br = old.body
				}
				newExtra := 0
				if br != nil && len(br) < len(f.body) {
					newExtra = len(br)
				}
				// Best compression can add a copy the fast pass did not keep.
				// Only store it while the cache budget still has room.
				if newExtra > oldExtra {
					if !reserveCache(uint64(newExtra - oldExtra)) {
						continue
					}
				} else if oldExtra > newExtra {
					releaseCache(uint64(oldExtra - newExtra))
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

// usableMemory is the RAM this process can use: the smaller of physical
// memory and the cgroup limit. Physical memory is MemTotal on Linux and the
// machine's RAM on macOS, FreeBSD and Windows. The cgroup limit is cgroup v2
// memory.max, or, when that file is not present, cgroup v1
// memory.limit_in_bytes (a huge unlimited sentinel is ignored). When neither
// can be read, it falls back to fallbackUsableMemory. GOMEMLIMIT, the
// connection cap and the default CACHE_MEMORY are all taken from this value.
func usableMemory() uint64 {
	total := physicalMemory()
	limit := cgroupMemoryLimit()
	switch {
	case total == 0 && limit == 0:
		return fallbackUsableMemory
	case total == 0:
		return limit
	case limit == 0 || limit > total:
		return total
	default:
		return limit
	}
}

func cacheRemaining() uint64 {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cachedRAM >= cacheBudget {
		return 0
	}
	return cacheBudget - cachedRAM
}

func reserveCache(n uint64) bool {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if n > cacheBudget || cachedRAM > cacheBudget-n {
		return false
	}
	cachedRAM += n
	return true
}

func releaseCache(n uint64) {
	if n == 0 {
		return
	}
	cacheMu.Lock()
	if n > cachedRAM {
		cachedRAM = 0
	} else {
		cachedRAM -= n
	}
	cacheMu.Unlock()
}

// connectionCap is MAX_CONNECTIONS. Unset or 0 means no cap. A positive
// value is how many connections are served at once.
func connectionCap() int {
	v := os.Getenv("MAX_CONNECTIONS")
	if v == "" {
		return 0
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		log.Fatalf("bad MAX_CONNECTIONS: %v", err)
	}
	if n == 0 {
		return 0
	}
	if n > uint64(math.MaxInt) {
		log.Fatalf("bad MAX_CONNECTIONS: %d", n)
	}
	return int(n)
}

func connectionCapLabel(n int) string {
	if n <= 0 {
		return "unlimited"
	}
	return strconv.Itoa(n)
}

// applyMemoryLimit sets the soft heap cap to half of usable memory when
// GOMEMLIMIT is not already set, and returns the limit that is in effect.
func applyMemoryLimit(usable uint64) int64 {
	prev := debug.SetMemoryLimit(-1)
	if os.Getenv("GOMEMLIMIT") != "" {
		return prev
	}
	limit := usable / 2
	if limit == 0 {
		limit = 1
	}
	if limit > uint64(math.MaxInt64) {
		limit = uint64(math.MaxInt64)
	}
	debug.SetMemoryLimit(int64(limit))
	return int64(limit)
}

// listen builds the TCP listener. On Linux, TCP_NOTSENT_LOWAT and
// TCP_USER_TIMEOUT are set on the socket before bind. Accepted sockets
// inherit both.
func listen(addr string) (net.Listener, error) {
	var lc net.ListenConfig
	configureListener(&lc)
	return lc.Listen(context.Background(), "tcp", addr)
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envUint(key string, fallback int) uint64 {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			log.Fatalf("bad %s: %v", key, err)
		}
		return n
	}
	return uint64(fallback)
}

// envBytes reads a byte size. A bare integer is bytes. A suffix of K, M, G
// or T (optional B or iB, any case) is a multiple of 1024, so 512M is
// 512 MiB and 2G is 2 GiB.
func envBytes(key string, fallback uint64) uint64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := parseByteSize(v)
	if err != nil {
		log.Fatalf("bad %s: %v", key, err)
	}
	return n
}

func parseByteSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	n, err := strconv.ParseUint(s[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	suffix := strings.TrimSpace(s[i:])
	if suffix == "" {
		return n, nil
	}
	var mult uint64
	switch strings.ToUpper(suffix) {
	case "K", "KB", "KI", "KIB":
		mult = 1 << 10
	case "M", "MB", "MI", "MIB":
		mult = 1 << 20
	case "G", "GB", "GI", "GIB":
		mult = 1 << 30
	case "T", "TB", "TI", "TIB":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n > math.MaxUint64/mult {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}

package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log"
	"math"
	"mime"
	"net/url"
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

	"github.com/andybalholm/brotli"
	"github.com/gofiber/fiber/v3"
	"github.com/klauspost/compress/zstd"
)

var (
	siteRoot   = envStr("ROOT", "/root/www")
	listenAddr = ":" + envStr("PORT", "80")
)

const (
	htmlCacheControl  = "no-cache, must-revalidate" // stored but re-checked every visit, so ETag/304 works
	assetCacheControl = "public, max-age=31536000, immutable, no-transform"

	// Files are compressed ahead of time, never per request, and in two steps so
	// that a restart never keeps the site offline. Before the server starts
	// listening, every file is compressed at the fast levels. Then, while the
	// site is already online, every file is compressed again at the strongest
	// levels in the background and swapped in. Visitors always get a compressed
	// file; shortly after startup they get the smallest one possible.
	zstdFast, zstdBest     = zstd.SpeedDefault, zstd.SpeedBestCompression
	brotliFast, brotliBest = brotli.DefaultCompression, brotli.BestCompression
	gzipFast, gzipBest     = gzip.DefaultCompression, gzip.BestCompression

	// Brotli looks for repeats inside a window, given as a power of two (64 KB
	// to 4 MB here). Each file gets the smallest window that covers it; see
	// brotliWindow.
	brotliMinWindow, brotliMaxWindow = 16, 22

	// Compressing leaves a lot of short-lived garbage behind. While the files
	// are loaded and compressed, Go's collector runs at this setting instead
	// of its usual 100, which keeps memory use low during that work.
	startupGCPercent = 25

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

// Bits of the index into file.pick: which encodings the client accepts.
const (
	acceptZstd = 1 << iota
	acceptBr
	acceptGzip
)

// One way of sending a file: the bytes, and the Content-Encoding they need
// ("" for the original bytes).
type variant struct {
	body []byte
	enc  string
}

type file struct {
	ctype, cc, etag string
	body            []byte // the original bytes

	// pick[mask] is the smallest version of the file that a client accepting
	// the encodings in mask can read. pick[0] is always the original bytes.
	// The whole table is replaced at once when the background step finishes
	// a file, so a request sees either the old table or the new one.
	pick atomic.Pointer[[8]variant]

	// True when the file has no compressed versions (video, audio, images,
	// PDFs, ...). Only those are served in byte ranges, which is what video
	// and audio players need for seeking, and what Safari needs to play at all.
	ranges bool
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
	// The server logs one more line when the background compression is done.
	// If its output was piped into another program and that program has
	// ended, Go's default is to stop the server at the next line it logs.
	// Ignoring the signal makes that line harmless.
	signal.Ignore(syscall.SIGPIPE)

	normalGCPercent := debug.SetGCPercent(startupGCPercent)

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstdFast))
	if err != nil {
		log.Fatalf("zstd init: %v", err)
	}
	load(siteRoot, enc)
	_ = enc.Close()

	if len(files) == 0 {
		log.Fatalf("no files found under %s", siteRoot)
	}
	log.Printf("loaded %d files from %s", len(files), siteRoot)
	go func() {
		// On a server with a single CPU core, Go runs one thing at a time, so
		// the compressing would hold up every visitor for a moment, over and
		// over. With a second thread the operating system can switch to a
		// visitor at once. It is taken away again when the work is done. With
		// more cores the setting is left alone, so that Go keeps following
		// the number of CPUs the server is given.
		if procs := runtime.GOMAXPROCS(0); procs < 2 {
			runtime.GOMAXPROCS(2)
			defer runtime.GOMAXPROCS(procs)
		}
		recompress()

		// The compressing is over. Put the collector back to normal and hand
		// the memory that was only needed for it back to the operating system.
		debug.SetGCPercent(normalGCPercent)
		debug.FreeOSMemory()
	}()

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
		c.Set("Vary", "Accept-Encoding")
		return c.SendStatus(fiber.StatusNotModified)
	}

	// Every client gets the smallest version it said it can read. A client that
	// sends no Accept-Encoding at all gets the original bytes.
	v := &f.pick.Load()[acceptMask(c)]
	body := v.body

	h := &c.Response().Header
	if f.ranges {
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
	}

	h.Set("ETag", f.etag)
	h.Set("Cache-Control", f.cc)
	h.Set("Vary", "Accept-Encoding")
	securityHeaders(c)

	h.SetContentType(f.ctype)
	if v.enc != "" {
		h.Set("Content-Encoding", v.enc)
	}
	return c.Send(body)
}

// acceptMask reads which encodings the client accepts, as the index into a
// file's pick table.
func acceptMask(c fiber.Ctx) int {
	mask := 0
	if ae := c.Get("Accept-Encoding"); ae != "" {
		if strings.Contains(ae, "zstd") {
			mask |= acceptZstd
		}
		if strings.Contains(ae, "br") {
			mask |= acceptBr
		}
		if strings.Contains(ae, "gzip") {
			mask |= acceptGzip
		}
	}
	return mask
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
	v := &f.pick.Load()[acceptMask(c)]
	c.Set("Vary", "Accept-Encoding")
	securityHeaders(c)
	c.Response().Header.SetContentType(f.ctype)
	if v.enc != "" {
		c.Set("Content-Encoding", v.enc)
	}
	return c.Status(fiber.StatusNotFound).Send(v.body)
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

func load(root string, enc *zstd.Encoder) {
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
		add(filepath.ToSlash(rel), body, enc)
		return nil
	})
}

func add(rel string, body []byte, enc *zstd.Encoder) {
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

	f := &file{ctype: ctype, cc: cc, etag: etag, body: body}
	var zs, br, gz []byte
	if compressible(ctype) {
		zs = smaller(enc.EncodeAll(body, make([]byte, 0, len(body)/2)), body)
		br = smaller(brotliOf(body, brotliFast), body)
		gz = smaller(gzipOf(body, gzipFast), body)
	}
	f.pick.Store(pickTable(body, zs, br, gz))
	f.ranges = zs == nil && br == nil && gz == nil
	files[rel] = f
}

// recompress is the background step: it compresses every compressible file
// again at the strongest levels and swaps the result in. It uses half of the
// CPU cores so the server stays responsive while it runs.
func recompress() {
	workers := max(1, runtime.GOMAXPROCS(0)/2)
	// One encoder that the workers take turns with. At this level each one
	// needs about 50 MB, and left alone the library sets up one per CPU core.
	// Sharing it costs little time: Brotli is what takes long here.
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstdBest), zstd.WithEncoderConcurrency(1))
	if err != nil {
		log.Printf("zstd init: %v (keeping the fast compression)", err)
		return
	}
	defer enc.Close()

	start := time.Now()
	work := make(chan *file)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range work {
				old := f.pick.Load()
				// Per encoding, keep whichever is smaller: the new result or
				// the one from startup.
				zs := smaller(enc.EncodeAll(f.body, make([]byte, 0, len(f.body)/2)), old[acceptZstd].body)
				br := smaller(brotliOf(f.body, brotliBest), old[acceptBr].body)
				gz := smaller(gzipOf(f.body, gzipBest), old[acceptGzip].body)
				f.pick.Store(pickTable(f.body, orOld(zs, old[acceptZstd]), orOld(br, old[acceptBr]), orOld(gz, old[acceptGzip])))
			}
		}()
	}
	n := 0
	for _, f := range files {
		if !f.ranges {
			work <- f
			n++
		}
	}
	close(work)
	wg.Wait()
	log.Printf("recompressed %d files at the strongest levels in %s", n, time.Since(start).Round(time.Millisecond))
}

// smaller returns compressed only if it is non-empty and smaller than than,
// otherwise nil. What it returns is a copy that takes exactly its own size in
// memory: the buffer a compressor writes into is much larger than its result,
// and would otherwise stay in memory for as long as the server runs.
func smaller(compressed, than []byte) []byte {
	if len(compressed) > 0 && len(compressed) < len(than) {
		return bytes.Clone(compressed)
	}
	return nil
}

// orOld returns the new compressed bytes, or the ones already in use for that
// encoding when the new attempt was not an improvement.
func orOld(compressed []byte, old variant) []byte {
	if compressed != nil || old.enc == "" {
		return compressed
	}
	return old.body
}

// pickTable works out, for every combination of encodings a client can accept,
// which version of the file is smallest. A nil version does not exist.
func pickTable(body, zs, br, gz []byte) *[8]variant {
	var t [8]variant
	for mask := range t {
		best := variant{body: body}
		// Ties go to the first one listed: br, then zstd, then gzip.
		for _, cand := range []struct {
			bit  int
			body []byte
			enc  string
		}{{acceptBr, br, "br"}, {acceptZstd, zs, "zstd"}, {acceptGzip, gz, "gzip"}} {
			if mask&cand.bit != 0 && cand.body != nil && len(cand.body) < len(best.body) {
				best = variant{body: cand.body, enc: cand.enc}
			}
		}
		t[mask] = best
	}
	return &t
}

// Text-like formats, plus WebAssembly and uncompressed fonts. Images, video,
// audio and woff/woff2 fonts are already compressed, so compressing them again
// would only waste time.
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

func brotliOf(b []byte, quality int) []byte {
	buf := bytes.NewBuffer(make([]byte, 0, len(b)/2))
	w := brotli.NewWriterOptions(buf, brotli.WriterOptions{Quality: quality, LGWin: brotliWindow(len(b))})
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

// brotliWindow returns the window for a file of the given size: just large
// enough to reach back over the whole file. A larger window cannot find
// anything more and only costs memory, a lot of it at the strongest level.
func brotliWindow(size int) int {
	lgwin := brotliMinWindow
	for lgwin < brotliMaxWindow && 1<<lgwin-16 < size {
		lgwin++
	}
	return lgwin
}

func gzipOf(b []byte, level int) []byte {
	buf := bytes.NewBuffer(make([]byte, 0, len(b)/2))
	w, err := gzip.NewWriterLevel(buf, level)
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

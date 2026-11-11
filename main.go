// PhoneDrop — รับไฟล์จากมือถือเข้าคอมผ่านเครือข่ายเดียวกัน
//
// โปรแกรมเดียวจบ: เซิร์ฟเวอร์รับไฟล์ + ไอคอนถาดระบบ
// ไฟล์ถูกเขียนลงดิสก์แบบ byte-for-byte จากที่มือถือส่งมา ไม่มีการแปลงหรือบีบอัด
// หน้าต่าง QR ใช้เบราว์เซอร์ที่มีอยู่ในเครื่อง (โหมดแอป ไม่มีแถบที่อยู่) จึงไม่กิน RAM ตอนปิดหน้าต่าง
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"fyne.io/systray"
	"github.com/gen2brain/beeep"
	"github.com/ncruces/zenity"
	qrcode "github.com/skip2/go-qrcode"
)

//go:embed public/phone.html public/desktop.html assets/icon.png assets/icon.ico
var assets embed.FS

const defaultPort = 5050

const (
	minTokenLen   = 16                // รหัสในลิงก์ที่สั้นกว่านี้ (จากเวอร์ชันเก่า) จะถูกสร้างใหม่
	maxFails      = 10                // เดารหัสผิดได้กี่ครั้งก่อนถูกพัก
	blockDuration = time.Minute       // พักนานเท่าไร
	diskReserve   = 500 * 1024 * 1024 // เหลือพื้นที่ดิสก์ไว้อย่างน้อยเท่านี้เสมอ
)

// newToken: สุ่ม 80 บิต (16 ตัวอักษร) — เดาไม่ได้ในทางปฏิบัติ
func newToken() string {
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
}

type failState struct {
	count int
	last  time.Time
	until time.Time
}

// ชื่อไฟล์ชั่วคราวระหว่างรับ — ใช้นามสกุลเฉพาะของเรา จะได้ไม่ชนกับไฟล์ .part ของโปรแกรมอื่น
const tempSuffix = ".pdpart"

// จำกัดชื่อไฟล์ที่ 180 ไบต์: Linux รับได้ 255 ไบต์ต่อชื่อ และอักษรไทยใช้ 3 ไบต์ต่อตัว
// (เผื่อที่ให้ " (n)" กับ tempSuffix)
const maxNameBytes = 180

// freeSpaceFn แยกไว้ให้ชุดทดสอบจำลองดิสก์เต็มได้
var freeSpaceFn = freeSpace

// ---------- การตั้งค่า ----------

type config struct {
	Token    string `json:"token"`
	Dir      string `json:"dir"`
	Notify   bool   `json:"notify"`
	LastPort int    `json:"lastPort"` // พอร์ตหน้าจอคอม (127.0.0.1) ของตัวที่เปิดอยู่
	LanPort  int    `json:"lanPort"`  // พอร์ต HTTPS ที่มือถือใช้
}

type fileInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Time int64  `json:"time"`
	path string
}

type app struct {
	mu        sync.Mutex
	cfg       config
	cfgPath   string
	lanPort   int    // HTTPS สำหรับมือถือ
	localPort int    // HTTP บน 127.0.0.1 สำหรับหน้าจอคอม
	certFP    string // ลายนิ้วมือใบรับรอง
	received  []fileInfo
	inFlight  map[string]bool
	subs      map[chan []byte]bool
	quitting  bool
	fails     map[string]*failState // นับการเดารหัสผิดต่อ IP

	notifyBatch []fileInfo
	notifyTimer *time.Timer
}

func defaultDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads", "PhoneDrop")
}

// loadConfig คืน true ถ้าเป็นการเปิดครั้งแรก
func (a *app) loadConfig() bool {
	first := true
	if b, err := os.ReadFile(a.cfgPath); err == nil && json.Unmarshal(b, &a.cfg) == nil && a.cfg.Token != "" {
		first = false
	}
	if first {
		a.cfg = config{Token: newToken(), Notify: true}
	} else if len(a.cfg.Token) < minTokenLen {
		a.cfg.Token = newToken() // รหัสจากเวอร์ชันเก่าสั้นเกินไป — ต้องสแกน QR ใหม่หนึ่งครั้ง
	}
	if a.cfg.Dir == "" {
		a.cfg.Dir = defaultDir()
	}
	return first
}

func (a *app) saveConfig() {
	a.mu.Lock()
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	a.mu.Unlock()
	os.MkdirAll(filepath.Dir(a.cfgPath), 0o755)
	os.WriteFile(a.cfgPath, b, 0o600)
}

func (a *app) dir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Dir
}

// ---------- ชื่อไฟล์ ----------

var (
	badChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
	reserved = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[1-9]|lpt[1-9])(\..*)?$`)
)

func safeName(raw string) string {
	name := path.Base(strings.ReplaceAll(raw, `\`, "/"))
	if name == "/" {
		name = ""
	}
	name = badChars.ReplaceAllString(name, "_")
	name = strings.TrimSpace(strings.TrimRight(name, ". "))
	if name == "" || reserved.MatchString(name) {
		name = fmt.Sprintf("file_%d", time.Now().UnixMilli())
	}
	if len(name) > maxNameBytes {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		stem := truncateUTF8(name[:len(name)-len(ext)], maxNameBytes-len(ext))
		name = strings.TrimRight(stem, ". ") + ext
	}
	return name
}

// truncateUTF8 ตัดสตริงให้ไม่เกิน n ไบต์โดยไม่ตัดกลางตัวอักษร
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// reserve จองชื่อไฟล์ที่ไม่ซ้ำ: IMG_0001.jpg -> IMG_0001 (1).jpg
func (a *app) reserve(dir, name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := 0; ; i++ {
		p := filepath.Join(dir, name)
		if i > 0 {
			p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		}
		if !a.inFlight[p] && !exists(p) && !exists(p+tempSuffix) {
			a.inFlight[p] = true
			return p
		}
	}
}

// cleanTemp ลบไฟล์ชั่วคราวที่ค้างจากการรับไฟล์ที่ไม่จบ (เช่น ไฟดับ) — เรียกตอนเริ่มโปรแกรมเท่านั้น
func (a *app) cleanTemp() {
	left, _ := filepath.Glob(filepath.Join(a.dir(), "*"+tempSuffix))
	for _, f := range left {
		os.Remove(f)
	}
}

// ---------- เครือข่าย ----------

// primaryAddress หา IP ของการ์ดเครือข่ายที่ใช้ออกเน็ตจริง (ไม่ส่งแพ็กเก็ต แค่ถาม routing table)
func primaryAddress() string {
	c, err := net.Dial("udp4", "1.1.1.1:53")
	if err != nil {
		return ""
	}
	defer c.Close()
	if u, ok := c.LocalAddr().(*net.UDPAddr); ok && !u.IP.IsUnspecified() {
		return u.IP.String()
	}
	return ""
}

func lanAddresses() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, ad := range addrs {
		ipn, ok := ad.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ip.String())
	}
	return out
}

// phoneURLs คำนวณใหม่ทุกครั้ง เผื่อคอมเปลี่ยน Wi-Fi ระหว่างที่โปรแกรมเปิดค้างไว้
func (a *app) phoneURLs() (urls []string) {
	seen := map[string]bool{}
	for _, ip := range append([]string{primaryAddress()}, lanAddresses()...) {
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		urls = append(urls, fmt.Sprintf("https://%s:%d/s/%s", ip, a.lanPort, a.cfg.Token))
	}
	return urls
}

func (a *app) localURL() string { return fmt.Sprintf("http://localhost:%d/", a.localPort) }

// ---------- แจ้งหน้าจอคอมแบบสด (Server-Sent Events) ----------

func (a *app) broadcast(v any) {
	b, _ := json.Marshal(v)
	a.mu.Lock()
	defer a.mu.Unlock()
	for ch := range a.subs {
		select {
		case ch <- b:
		default:
		}
	}
}

func (a *app) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no stream", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, ": ok\n\n")
	fl.Flush()

	ch := make(chan []byte, 32)
	a.mu.Lock()
	a.subs[ch] = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.subs, ch)
		a.mu.Unlock()
	}()
	for {
		select {
		case m := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", m)
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// ---------- รับไฟล์ ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (a *app) upload(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.URL.Query().Get("name"))
	dir := a.dir()
	os.MkdirAll(dir, 0o755)
	final := a.reserve(dir, name)
	defer func() {
		a.mu.Lock()
		delete(a.inFlight, final)
		a.mu.Unlock()
	}()
	tmp := final + tempSuffix

	// กันดิสก์เต็ม: รับได้ไม่เกินพื้นที่ว่างลบส่วนที่กันไว้
	var body io.Reader = r.Body
	if free, ferr := freeSpaceFn(dir); ferr == nil {
		var limit int64
		if free > diskReserve {
			limit = int64(free - diskReserve)
		}
		if r.ContentLength > limit {
			writeJSON(w, http.StatusInsufficientStorage, map[string]any{"ok": false, "error": "พื้นที่ดิสก์ไม่พอ"})
			return
		}
		body = http.MaxBytesReader(w, r.Body, limit)
	}

	n, err := func() (int64, error) {
		f, err := os.Create(tmp)
		if err != nil {
			return 0, err
		}
		n, err := io.Copy(f, body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return n, err
	}()
	if err == nil && r.ContentLength >= 0 && n != r.ContentLength {
		err = fmt.Errorf("ได้รับ %d จาก %d ไบต์", n, r.ContentLength)
	}
	if err == nil {
		err = os.Rename(tmp, final)
	}
	if err != nil {
		os.Remove(tmp)
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// ติดป้ายก่อน แล้วค่อยตั้งวันที่ — การติดป้ายบน Windows ทำให้วันที่แก้ไขไฟล์เปลี่ยน
	markFromNetwork(final)
	// คงวันที่แก้ไขไฟล์เดิมจากมือถือไว้
	if ms, _ := strconv.ParseInt(r.URL.Query().Get("mtime"), 10, 64); ms > 0 {
		os.Chtimes(final, time.Now(), time.UnixMilli(ms))
	}

	info := fileInfo{Name: filepath.Base(final), Size: n, Time: time.Now().UnixMilli(), path: final}
	a.mu.Lock()
	a.received = append(a.received, info)
	a.mu.Unlock()
	a.broadcast(map[string]any{"type": "file", "file": info})
	a.queueNotify(info)
	writeJSON(w, 200, map[string]any{"ok": true, "name": info.Name, "size": n})
}

// queueNotify รวมแจ้งเตือนเมื่อส่งมาหลายไฟล์ติดกัน
func (a *app) queueNotify(f fileInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.cfg.Notify {
		return
	}
	a.notifyBatch = append(a.notifyBatch, f)
	if a.notifyTimer != nil {
		a.notifyTimer.Stop()
	}
	a.notifyTimer = time.AfterFunc(1500*time.Millisecond, func() {
		a.mu.Lock()
		files := a.notifyBatch
		a.notifyBatch = nil
		a.mu.Unlock()
		if len(files) == 0 {
			return
		}
		body := files[0].Name
		if len(files) > 1 {
			body = fmt.Sprintf("%s และอีก %d ไฟล์", files[0].Name, len(files)-1)
		}
		icon, _ := assets.ReadFile("assets/icon.png")
		beeep.Notify(fmt.Sprintf("ได้รับ %d ไฟล์จากมือถือ", len(files)), body, icon)
	})
}

// ---------- HTTP ----------

func remoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// hostAllowed กัน DNS rebinding: รับเฉพาะคำขอที่เรียกมาด้วยเลข IP ตรง ๆ หรือ localhost
// เว็บไซต์ภายนอกที่หลอกเบราว์เซอร์ให้ยิงเข้ามาจะมี Host เป็นชื่อโดเมนของมันเอง
func hostAllowed(r *http.Request) bool {
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	return strings.EqualFold(h, "localhost") || net.ParseIP(h) != nil
}

func sameString(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// blocked: IP นี้เดารหัสผิดเกินกำหนดและยังอยู่ในช่วงพักหรือไม่
func (a *app) blocked(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.fails[ip]
	return f != nil && time.Now().Before(f.until)
}

func (a *app) noteFail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.fails) > 4096 { // กันหน่วยความจำโต
		a.fails = map[string]*failState{}
	}
	f := a.fails[ip]
	if f == nil {
		f = &failState{}
		a.fails[ip] = f
	}
	if time.Since(f.last) > 10*time.Minute {
		f.count = 0 // ลืมความผิดพลาดเก่า ๆ
	}
	f.last = time.Now()
	f.count++
	if f.count >= maxFails {
		f.count = 0
		f.until = time.Now().Add(blockDuration)
	}
}

func serveAsset(w http.ResponseWriter, name, ctype string) {
	b, err := assets.ReadFile(name)
	if err != nil {
		http.Error(w, "missing "+name, 500)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

// tokenRoute จัดการคำขอที่มือถือใช้ — คืน true ถ้าจัดการแล้ว
func (a *app) tokenRoute(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	switch {
	case r.Method == "GET" && sameString(p, "/s/"+a.cfg.Token):
		serveAsset(w, "public/phone.html", "text/html; charset=utf-8")
	case r.Method == "PUT" && sameString(p, "/api/upload/"+a.cfg.Token):
		a.upload(w, r)
	case r.Method == "GET" && (p == "/icon.png" || p == "/favicon.ico" || strings.HasPrefix(p, "/apple-touch-icon")):
		serveAsset(w, "assets/icon.png", "image/png") // ไอคอนไม่ใช่ความลับ — ใช้ตอนเพิ่มไว้หน้าโฮมของมือถือ
	default:
		return false
	}
	return true
}

// serveLAN: ช่องทางสำหรับมือถือ (HTTPS, เปิดรับจากเครือข่าย) — มีแค่หน้าส่งไฟล์กับการรับไฟล์
func (a *app) serveLAN(w http.ResponseWriter, r *http.Request) {
	if !hostAllowed(r) {
		http.Error(w, "forbidden host", 403)
		return
	}
	ip := remoteIP(r)
	local := ip != nil && ip.IsLoopback()
	key := ""
	if ip != nil {
		key = ip.String()
	}
	if !local && a.blocked(key) {
		http.Error(w, "ลองผิดหลายครั้งเกินไป กรุณารอสักครู่", http.StatusTooManyRequests)
		return
	}
	if a.tokenRoute(w, r) {
		return
	}
	// นับเฉพาะคำขอที่เป็นการลองรหัส — เบราว์เซอร์มือถือขอไฟล์อื่นเองอยู่บ่อย ๆ ไม่ควรโดนนับไปด้วย
	if p := r.URL.Path; !local && (strings.HasPrefix(p, "/s/") || strings.HasPrefix(p, "/api/upload/")) {
		a.noteFail(key)
	}
	http.NotFound(w, r)
}

// serveLocal: ช่องทางสำหรับหน้าจอคอม (HTTP ธรรมดา ผูกกับ 127.0.0.1 เท่านั้น ไม่ออกเครือข่าย)
func (a *app) serveLocal(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	token := a.cfg.Token

	if ip := remoteIP(r); ip == nil || !ip.IsLoopback() || !hostAllowed(r) {
		http.Error(w, "forbidden", 403)
		return
	}
	if a.tokenRoute(w, r) {
		return
	}

	switch {
	case r.Method == "GET" && p == "/":
		serveAsset(w, "public/desktop.html", "text/html; charset=utf-8")
	case r.Method == "GET" && p == "/icon.png":
		serveAsset(w, "assets/icon.png", "image/png")
	case r.Method == "GET" && p == "/api/ping":
		fmt.Fprint(w, "phonedrop")
	case r.Method == "GET" && p == "/api/info":
		urls := a.phoneURLs()
		a.mu.Lock()
		files := append([]fileInfo{}, a.received...)
		dir := a.cfg.Dir
		a.mu.Unlock()
		url := ""
		if len(urls) > 0 {
			url = urls[0]
		}
		writeJSON(w, 200, map[string]any{
			"url": url, "urls": urls, "online": len(urls) > 0,
			"dir": dir, "token": token, "files": files, "fingerprint": a.certFP,
		})
	case r.Method == "GET" && p == "/api/qr.png":
		urls := a.phoneURLs()
		if len(urls) == 0 {
			http.Error(w, "offline", 503)
			return
		}
		png, err := qrcode.Encode(urls[0], qrcode.Medium, 640)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(png)
	case r.Method == "GET" && p == "/api/events":
		a.events(w, r)
	case r.Method == "POST" && p == "/api/open/"+token:
		openFolder(a.dir())
		writeJSON(w, 200, map[string]any{"ok": true})
	case r.Method == "POST" && p == "/api/show/"+token:
		go a.showWindow()
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

// ---------- หน้าต่าง / ระบบ ----------

func start(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func openDefault(target string) {
	switch runtime.GOOS {
	case "windows":
		start("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		start("open", target)
	default:
		start("xdg-open", target)
	}
}

func openFolder(dir string) {
	os.MkdirAll(dir, 0o755)
	if runtime.GOOS == "windows" {
		start("explorer.exe", dir)
		return
	}
	openDefault(dir)
}

// appBrowser หาเบราว์เซอร์ตระกูล Chromium ที่เปิด "โหมดแอป" (หน้าต่างไม่มีแถบที่อยู่) ได้
func appBrowser() string {
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		for _, root := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles"), os.Getenv("LocalAppData")} {
			if root == "" {
				continue
			}
			candidates = append(candidates,
				filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(root, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
			)
		}
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	default:
		for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "brave-browser"} {
			if p, err := exec.LookPath(n); err == nil {
				return p
			}
		}
	}
	for _, c := range candidates {
		if exists(c) {
			return c
		}
	}
	return ""
}

// showWindow เปิดหน้าต่าง QR — ถ้ามีหน้าต่างเดิมเปิดอยู่ ให้ปิดตัวเองก่อน จะได้ไม่ซ้อนกัน
func (a *app) showWindow() {
	a.mu.Lock()
	open := len(a.subs) > 0
	a.mu.Unlock()
	if open {
		a.broadcast(map[string]any{"type": "close"})
		time.Sleep(250 * time.Millisecond)
	}
	if b := appBrowser(); b != "" {
		if start(b, "--app="+a.localURL(), "--window-size=940,640") == nil {
			return
		}
	}
	openDefault(a.localURL())
}

func (a *app) chooseDir() {
	dir, err := zenity.SelectFile(
		zenity.Directory(),
		zenity.Title("เลือกโฟลเดอร์สำหรับเก็บไฟล์ที่รับ"),
		zenity.Filename(a.dir()+string(filepath.Separator)),
	)
	if err != nil || dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		zenity.Error("ใช้โฟลเดอร์นี้ไม่ได้: "+err.Error(), zenity.Title("PhoneDrop"))
		return
	}
	a.mu.Lock()
	a.cfg.Dir = dir
	a.mu.Unlock()
	a.saveConfig()
	a.broadcast(map[string]any{"type": "dir", "dir": dir})
}

// ---------- ถาดระบบ ----------

func (a *app) trayReady() {
	iconName := "assets/icon.png"
	if runtime.GOOS == "windows" {
		iconName = "assets/icon.ico"
	}
	icon, _ := assets.ReadFile(iconName)
	systray.SetIcon(icon)
	systray.SetTooltip("PhoneDrop — พร้อมรับไฟล์จากมือถือ")
	systray.SetOnTapped(func() { go a.showWindow() })

	mShow := systray.AddMenuItem("แสดง QR code", "")
	mOpen := systray.AddMenuItem("เปิดโฟลเดอร์ไฟล์ที่รับ", "")
	mDir := systray.AddMenuItem("เปลี่ยนโฟลเดอร์ปลายทาง…", "")
	systray.AddSeparator()
	mAuto := systray.AddMenuItemCheckbox("เปิดพร้อมเครื่อง", "", getAutoStart())
	mNotify := systray.AddMenuItemCheckbox("แจ้งเตือนเมื่อได้รับไฟล์", "", a.cfg.Notify)
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("ออกจาก PhoneDrop", "")

	go func() {
		for {
			select {
			case <-mShow.ClickedCh:
				go a.showWindow()
			case <-mOpen.ClickedCh:
				openFolder(a.dir())
			case <-mDir.ClickedCh:
				go a.chooseDir()
			case <-mAuto.ClickedCh:
				on := !mAuto.Checked()
				if setAutoStart(on) == nil {
					if on {
						mAuto.Check()
					} else {
						mAuto.Uncheck()
					}
				}
			case <-mNotify.ClickedCh:
				a.mu.Lock()
				a.cfg.Notify = !a.cfg.Notify
				on := a.cfg.Notify
				a.mu.Unlock()
				if on {
					mNotify.Check()
				} else {
					mNotify.Uncheck()
				}
				a.saveConfig()
			case <-mQuit.ClickedCh:
				a.broadcast(map[string]any{"type": "close"})
				time.Sleep(150 * time.Millisecond)
				a.mu.Lock()
				a.quitting = true
				a.mu.Unlock()
				systray.Quit()
				return
			}
		}
	}()
}

// ---------- เริ่มทำงาน ----------

func fatal(msg string) {
	zenity.Error(msg, zenity.Title("PhoneDrop"))
	os.Exit(1)
}

// alreadyRunning: ถ้ามี PhoneDrop เปิดอยู่แล้ว สั่งให้ตัวเดิมโชว์หน้าต่างแทน
func alreadyRunning(cfg config, show bool) bool {
	if cfg.LastPort == 0 {
		return false
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", cfg.LastPort)
	client := &http.Client{Timeout: 700 * time.Millisecond}
	resp, err := client.Get(base + "/api/ping")
	if err != nil {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32))
	resp.Body.Close()
	if string(body) != "phonedrop" {
		return false
	}
	if !show {
		return true
	}
	resp, err = client.Post(base+"/api/show/"+cfg.Token, "text/plain", nil)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200 // token ไม่ตรง = เป็นของผู้ใช้อื่น ให้เปิดตัวใหม่
}

func main() {
	hidden, noAutoStart := false, false
	cfgDir := ""
	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--hidden":
			hidden = true
		case "--no-autostart":
			noAutoStart = true
		case "--config":
			if i+1 < len(os.Args) {
				i++
				cfgDir = os.Args[i]
			}
		}
	}
	if cfgDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			base, _ = os.UserHomeDir()
		}
		cfgDir = filepath.Join(base, "PhoneDrop")
	}

	a := &app{
		cfgPath:  filepath.Join(cfgDir, "config.json"),
		inFlight: map[string]bool{},
		subs:     map[chan []byte]bool{},
		fails:    map[string]*failState{},
	}
	firstRun := a.loadConfig()

	if alreadyRunning(a.cfg, !hidden) {
		return
	}

	// โฟลเดอร์ที่ตั้งไว้ใช้ไม่ได้ (เช่น ถอดไดรฟ์ออกไปแล้ว) — กลับไปใช้โฟลเดอร์ปริยาย
	if err := os.MkdirAll(a.cfg.Dir, 0o755); err != nil {
		a.cfg.Dir = defaultDir()
		if err := os.MkdirAll(a.cfg.Dir, 0o755); err != nil {
			fatal("สร้างโฟลเดอร์เก็บไฟล์ไม่ได้: " + err.Error())
		}
	}

	cert, fp, err := loadOrCreateCert(cfgDir, lanAddresses())
	if err != nil {
		fatal("สร้างใบรับรอง HTTPS ไม่ได้: " + err.Error())
	}
	a.certFP = fp

	// ช่องทางหน้าจอคอม: HTTP บน 127.0.0.1 เท่านั้น (พอร์ตสุ่ม)
	localLn, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		fatal("เปิดพอร์ตภายในเครื่องไม่ได้: " + err.Error())
	}
	a.localPort = localLn.Addr().(*net.TCPAddr).Port

	// ช่องทางมือถือ: HTTPS เปิดรับจากเครือข่าย
	var lanLn net.Listener
	for p := defaultPort; p < defaultPort+20; p++ {
		if lanLn, err = net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", p)); err == nil {
			a.lanPort = p
			break
		}
		if p == defaultPort {
			// พอร์ตหลักไม่ว่าง: อาจเป็น PhoneDrop อีกตัวที่เพิ่งเปิดพร้อมกัน — รอสักครู่แล้วเช็กอีกครั้ง
			time.Sleep(700 * time.Millisecond)
			var other config
			if b, rerr := os.ReadFile(a.cfgPath); rerr == nil && json.Unmarshal(b, &other) == nil && alreadyRunning(other, !hidden) {
				return
			}
		}
	}
	if lanLn == nil {
		fatal("เปิดพอร์ตรับไฟล์ไม่ได้: " + err.Error())
	}
	a.cfg.LastPort = a.localPort
	a.cfg.LanPort = a.lanPort
	a.saveConfig()
	a.cleanTemp()

	beeep.AppName = "PhoneDrop"
	quiet := log.New(io.Discard, "", 0)
	// ไม่ตั้ง timeout ของเนื้อไฟล์ (วิดีโอใหญ่ ๆ ใช้เวลาส่งนาน) แต่จำกัดเวลาส่ง header กันการเปิดการเชื่อมต่อค้าง
	localSrv := &http.Server{Handler: http.HandlerFunc(a.serveLocal), ReadHeaderTimeout: 10 * time.Second, ErrorLog: quiet}
	lanSrv := &http.Server{
		Handler:           http.HandlerFunc(a.serveLAN),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          quiet,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		// HTTP/1.1 เท่านั้น: ส่งไฟล์ใหญ่ในวงแลนได้เร็วกว่า HTTP/2
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
	}
	go localSrv.Serve(localLn)
	go lanSrv.ServeTLS(lanLn, "", "")

	// ครั้งแรกหลังติดตั้ง: ตั้งให้เปิดพร้อมเครื่อง (ปิดได้จากเมนูที่ไอคอนถาดระบบ)
	if firstRun && !noAutoStart {
		setAutoStart(true)
	}
	if !hidden {
		go a.showWindow()
	}

	systray.Run(a.trayReady, func() {})

	// ถ้าถาดระบบใช้ไม่ได้ (เช่น Linux ที่ไม่มี tray) ให้รับไฟล์ต่อไป — ออกเฉพาะเมื่อผู้ใช้สั่งออกเอง
	a.mu.Lock()
	quitting := a.quitting
	a.mu.Unlock()
	if !quitting {
		select {}
	}
}

// PhoneDrop — รับไฟล์จากมือถือเข้าคอมผ่านเครือข่ายเดียวกัน
//
// โปรแกรมเดียวจบ: เซิร์ฟเวอร์รับไฟล์ + ไอคอนถาดระบบ
// ไฟล์ถูกเขียนลงดิสก์แบบ byte-for-byte จากที่มือถือส่งมา ไม่มีการแปลงหรือบีบอัด
// หน้าต่าง QR ใช้เบราว์เซอร์ที่มีอยู่ในเครื่อง (โหมดแอป ไม่มีแถบที่อยู่) จึงไม่กิน RAM ตอนปิดหน้าต่าง
package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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

	"fyne.io/systray"
	"github.com/gen2brain/beeep"
	"github.com/ncruces/zenity"
	qrcode "github.com/skip2/go-qrcode"
)

//go:embed public/phone.html public/desktop.html assets/icon.png assets/icon.ico
var assets embed.FS

const defaultPort = 5050

// ---------- การตั้งค่า ----------

type config struct {
	Token    string `json:"token"`
	Dir      string `json:"dir"`
	Notify   bool   `json:"notify"`
	LastPort int    `json:"lastPort"`
}

type fileInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Time int64  `json:"time"`
	path string
}

type app struct {
	mu       sync.Mutex
	cfg      config
	cfgPath  string
	port     int
	received []fileInfo
	inFlight map[string]bool
	subs     map[chan []byte]bool
	quitting bool

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
		buf := make([]byte, 4)
		rand.Read(buf)
		a.cfg = config{Token: hex.EncodeToString(buf), Notify: true}
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
	name = badChars.ReplaceAllString(name, "_")
	name = strings.TrimSpace(strings.TrimRight(name, ". "))
	if name == "" || reserved.MatchString(name) {
		name = fmt.Sprintf("file_%d", time.Now().UnixMilli())
	}
	if r := []rune(name); len(r) > 200 {
		ext := []rune(filepath.Ext(name))
		if len(ext) > 20 {
			ext = ext[:20]
		}
		name = string(r[:200-len(ext)]) + string(ext)
	}
	return name
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
		if !a.inFlight[p] && !exists(p) && !exists(p+".part") {
			a.inFlight[p] = true
			return p
		}
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
		urls = append(urls, fmt.Sprintf("http://%s:%d/s/%s", ip, a.port, a.cfg.Token))
	}
	return urls
}

func (a *app) localURL() string { return fmt.Sprintf("http://localhost:%d/", a.port) }

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
	tmp := final + ".part"

	n, err := func() (int64, error) {
		f, err := os.Create(tmp)
		if err != nil {
			return 0, err
		}
		n, err := io.Copy(f, r.Body)
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

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	token := a.cfg.Token

	// หน้าบนมือถือ (ต้องมี token จาก QR)
	if r.Method == "GET" && p == "/s/"+token {
		serveAsset(w, "public/phone.html", "text/html; charset=utf-8")
		return
	}
	if r.Method == "PUT" && p == "/api/upload/"+token {
		a.upload(w, r)
		return
	}

	// ส่วนที่เหลือใช้ได้จากเครื่องนี้เท่านั้น
	if !isLoopback(r) {
		http.Error(w, "กรุณาสแกน QR code บนหน้าจอคอมอีกครั้ง", 403)
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
			"dir": dir, "token": token, "files": files,
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

	var ln net.Listener
	var err error
	for p := defaultPort; p < defaultPort+20; p++ {
		if ln, err = net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", p)); err == nil {
			a.port = p
			break
		}
	}
	if ln == nil {
		fatal("เปิดพอร์ตรับไฟล์ไม่ได้: " + err.Error())
	}
	a.cfg.LastPort = a.port
	a.saveConfig()

	beeep.AppName = "PhoneDrop"
	go http.Serve(ln, a) // ไม่ตั้ง timeout: วิดีโอใหญ่ ๆ อาจใช้เวลาส่งนาน

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

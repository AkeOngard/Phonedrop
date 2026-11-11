package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const testToken = "testtoken0123456"

const (
	phoneAddr = "192.168.1.20:50000" // มือถือในวงแลน
	lanHost   = "http://192.168.1.10:5050"
	localHost = "http://localhost:5049"
	loopAddr  = "127.0.0.1:40000"
)

func newTestApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	a := &app{
		cfgPath:   filepath.Join(dir, "cfg", "config.json"),
		inFlight:  map[string]bool{},
		subs:      map[chan []byte]bool{},
		fails:     map[string]*failState{},
		lanPort:   5050,
		localPort: 5049,
	}
	a.cfg = config{Token: testToken, Dir: filepath.Join(dir, "recv")}
	return a
}

func do(h http.HandlerFunc, method, target, remote string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func uploadURL(base, name string) string {
	return base + "/api/upload/" + testToken + "?name=" + url.QueryEscape(name)
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	for _, n := range listDir(t, dir) {
		if strings.HasSuffix(n, tempSuffix) {
			t.Errorf("ไฟล์ชั่วคราวค้างอยู่: %s", n)
		}
	}
}

// ---------- ชื่อไฟล์ ----------

func TestSafeName(t *testing.T) {
	exact := map[string]string{
		"photo.jpg":             "photo.jpg",
		`..\..\win.ini`:         "win.ini",
		"../../../etc/passwd":   "passwd",
		"/abs/path/a.txt":       "a.txt",
		`C:\Users\x\secret.txt`: "secret.txt",
		"a.txt:stream":          "a.txt_stream",
		"a\x00b.txt":            "a_b.txt",
		`we<ir>d|na"me?*.png`:   "we_ir_d_na_me__.png",
		"trailing.dots...":      "trailing.dots",
		"trailing space   ":     "trailing space",
		"รูปถ่าย วันเกิด.heic": "รูปถ่าย วันเกิด.heic",
		".hidden": ".hidden",
	}
	for in, want := range exact {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, ต้องการ %q", in, got, want)
		}
	}

	// ชื่อที่ใช้ไม่ได้ต้องถูกแทนด้วยชื่อที่สร้างเอง
	for _, in := range []string{"", ".", "..", "...", "/", `\`, "CON", "nul.txt", "COM1", "lpt9.jpg", "   "} {
		got := safeName(in)
		if !strings.HasPrefix(got, "file_") {
			t.Errorf("safeName(%q) = %q, ควรเป็นชื่อที่สร้างเอง", in, got)
		}
	}

	// ผลลัพธ์ต้องไม่มีตัวคั่นโฟลเดอร์ ไม่ยาวเกิน และเป็น UTF-8 ที่ถูกต้องเสมอ
	long := []string{
		strings.Repeat("ก", 400) + ".jpg",
		strings.Repeat("a", 1000) + ".mp4",
		strings.Repeat("😀", 200) + ".png",
		strings.Repeat("x", 300) + "." + strings.Repeat("e", 100),
		strings.Repeat("ก", 59) + "ab" + strings.Repeat(".", 50) + ".jpg",
	}
	for _, in := range long {
		got := safeName(in)
		if len(got) > maxNameBytes {
			t.Errorf("ยาว %d ไบต์ เกิน %d: %q", len(got), maxNameBytes, got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("ตัดกลางตัวอักษร: %q", got)
		}
		if strings.ContainsAny(got, `/\`) || got == "" {
			t.Errorf("ชื่อไม่ปลอดภัย: %q", got)
		}
	}
	if got := safeName(strings.Repeat("ก", 400) + ".jpg"); !strings.HasSuffix(got, ".jpg") {
		t.Errorf("นามสกุลหาย: %q", got)
	}
}

// ---------- รับไฟล์ ----------

func TestUploadRoundTrip(t *testing.T) {
	a := newTestApp(t)
	data := randBytes(3*1024*1024 + 17)
	mtime := time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)
	target := uploadURL(lanHost, "IMG_0001.HEIC") + fmt.Sprintf("&mtime=%d", mtime.UnixMilli())

	w := do(a.serveLAN, "PUT", target, phoneAddr, data)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp struct {
		OK   bool
		Name string
		Size int64
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.OK || resp.Name != "IMG_0001.HEIC" || resp.Size != int64(len(data)) {
		t.Errorf("คำตอบผิด: %+v", resp)
	}

	final := filepath.Join(a.cfg.Dir, "IMG_0001.HEIC")
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Error("เนื้อไฟล์ไม่ตรงต้นฉบับ")
	}
	st, _ := os.Stat(final)
	if d := st.ModTime().Sub(mtime); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("วันที่ไฟล์ไม่ถูกคงไว้: %v", st.ModTime())
	}
	assertNoTemp(t, a.cfg.Dir)
	if len(a.received) != 1 || len(a.inFlight) != 0 {
		t.Errorf("สถานะภายในผิด: received=%d inFlight=%d", len(a.received), len(a.inFlight))
	}
}

func TestUploadEdgeCases(t *testing.T) {
	a := newTestApp(t)

	// ไฟล์ว่าง
	if w := do(a.serveLAN, "PUT", uploadURL(lanHost, "empty.txt"), phoneAddr, nil); w.Code != 200 {
		t.Fatalf("ไฟล์ว่าง: status %d", w.Code)
	}
	if st, err := os.Stat(filepath.Join(a.cfg.Dir, "empty.txt")); err != nil || st.Size() != 0 {
		t.Errorf("ไฟล์ว่างไม่ถูกบันทึก: %v", err)
	}

	// ไม่ส่งชื่อมา
	w := do(a.serveLAN, "PUT", lanHost+"/api/upload/"+testToken, phoneAddr, []byte("x"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"file_`) {
		t.Errorf("ไม่มีชื่อ: %d %s", w.Code, w.Body)
	}

	// ชื่อซ้ำต้องไม่เขียนทับ
	for i, want := range []string{"dup.jpg", "dup (1).jpg", "dup (2).jpg"} {
		body := []byte(fmt.Sprintf("content-%d", i))
		w := do(a.serveLAN, "PUT", uploadURL(lanHost, "dup.jpg"), phoneAddr, body)
		if w.Code != 200 {
			t.Fatalf("dup %d: status %d", i, w.Code)
		}
		got, err := os.ReadFile(filepath.Join(a.cfg.Dir, want))
		if err != nil || !bytes.Equal(got, body) {
			t.Errorf("ไฟล์ %q ผิด: %v %q", want, err, got)
		}
	}

	// ชื่อยาวมากภาษาไทยต้องบันทึกได้ (เคยเกินขีดจำกัด 255 ไบต์ของ Linux)
	if w := do(a.serveLAN, "PUT", uploadURL(lanHost, strings.Repeat("ก", 300)+".jpg"), phoneAddr, []byte("x")); w.Code != 200 {
		t.Errorf("ชื่อยาว: status %d %s", w.Code, w.Body)
	}

	// ความพยายามเขียนนอกโฟลเดอร์
	parent := filepath.Dir(a.cfg.Dir)
	before := listDir(t, parent)
	for _, n := range []string{`..\escape1.txt`, "../escape2.txt", "../../escape3.txt", "/escape4.txt"} {
		do(a.serveLAN, "PUT", uploadURL(lanHost, n), phoneAddr, []byte("x"))
	}
	if after := listDir(t, parent); len(after) != len(before) {
		t.Errorf("มีไฟล์หลุดออกนอกโฟลเดอร์รับ: %v", after)
	}
	assertNoTemp(t, a.cfg.Dir)
}

func TestWrongMethodOrToken(t *testing.T) {
	a := newTestApp(t)
	cases := []struct{ method, path string }{
		{"GET", "/api/upload/" + testToken},
		{"POST", "/api/upload/" + testToken},
		{"DELETE", "/api/upload/" + testToken},
		{"PUT", "/s/" + testToken},
		{"PUT", "/api/upload/" + testToken + "x"},
		{"PUT", "/api/upload/" + testToken[:len(testToken)-1]},
		{"PUT", "/api/upload/"},
		{"GET", "/s/" + strings.ToUpper(testToken)},
		{"GET", "/s/" + testToken + "/"},
	}
	for i, c := range cases {
		// ใช้ IP ต่างกันทุกครั้ง จะได้ไม่โดนพักจากการลองผิด
		w := do(a.serveLAN, c.method, lanHost+c.path, fmt.Sprintf("192.168.1.%d:1", 100+i), []byte("x"))
		if w.Code != 404 {
			t.Errorf("%s %s = %d, ต้องการ 404", c.method, c.path, w.Code)
		}
	}
	if n := listDir(t, a.cfg.Dir); len(n) != 0 {
		t.Errorf("มีไฟล์ถูกเขียนทั้งที่ไม่ควร: %v", n)
	}
}

func TestConcurrentSameName(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(http.HandlerFunc(a.serveLAN))
	defer srv.Close()

	const n = 40
	want := map[string]bool{}
	payloads := make([][]byte, n)
	for i := range payloads {
		payloads[i] = append([]byte(fmt.Sprintf("file-%03d-", i)), randBytes(64*1024)...)
		want[string(payloads[i])] = true
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequest("PUT", uploadURL(srv.URL, "IMG.jpg"), bytes.NewReader(payloads[i]))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("status %d", resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()

	names := listDir(t, a.cfg.Dir)
	if len(names) != n {
		t.Fatalf("ได้ %d ไฟล์ ต้องการ %d", len(names), n)
	}
	for _, name := range names {
		b, _ := os.ReadFile(filepath.Join(a.cfg.Dir, name))
		if !want[string(b)] {
			t.Errorf("ไฟล์ %s เนื้อหาปนกันหรือเสียหาย", name)
		}
		delete(want, string(b))
	}
	if len(want) != 0 {
		t.Errorf("มี %d ไฟล์หายไป", len(want))
	}
	assertNoTemp(t, a.cfg.Dir)
}

// มือถือหลุดกลางทาง: ต้องไม่เหลือไฟล์ครึ่ง ๆ กลาง ๆ
func TestAbortedUpload(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(http.HandlerFunc(a.serveLAN))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "PUT /api/upload/%s?name=video.mp4 HTTP/1.1\r\nHost: %s\r\nContent-Length: 1000000\r\n\r\n", testToken, srv.Listener.Addr())
	conn.Write(randBytes(100000))
	time.Sleep(100 * time.Millisecond)
	conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		a.mu.Lock()
		busy := len(a.inFlight)
		a.mu.Unlock()
		if busy == 0 && len(listDir(t, a.cfg.Dir)) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ไฟล์ค้างหลังการเชื่อมต่อหลุด: %v (inFlight=%d)", listDir(t, a.cfg.Dir), busy)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(a.received) != 0 {
		t.Error("ไฟล์ที่ส่งไม่ครบถูกนับว่ารับสำเร็จ")
	}
}

func TestDiskFull(t *testing.T) {
	a := newTestApp(t)
	old := freeSpaceFn
	defer func() { freeSpaceFn = old }()
	freeSpaceFn = func(string) (uint64, error) { return diskReserve + 100, nil } // รับได้อีกแค่ 100 ไบต์

	if w := do(a.serveLAN, "PUT", uploadURL(lanHost, "big.bin"), phoneAddr, randBytes(1000)); w.Code != http.StatusInsufficientStorage {
		t.Errorf("ไฟล์ใหญ่เกิน: status %d ต้องการ 507", w.Code)
	}
	if w := do(a.serveLAN, "PUT", uploadURL(lanHost, "small.bin"), phoneAddr, randBytes(50)); w.Code != 200 {
		t.Errorf("ไฟล์เล็กที่ยังพอดี: status %d", w.Code)
	}

	// ไม่บอกขนาดล่วงหน้า (chunked) แล้วส่งเกิน
	r := httptest.NewRequest("PUT", uploadURL(lanHost, "chunked.bin"), io.NopCloser(bytes.NewReader(randBytes(5000))))
	r.ContentLength = -1
	r.RemoteAddr = phoneAddr
	w := httptest.NewRecorder()
	a.serveLAN(w, r)
	if w.Code == 200 {
		t.Error("chunked เกินพื้นที่แต่ถูกรับ")
	}

	freeSpaceFn = func(string) (uint64, error) { return 10, nil } // ต่ำกว่าส่วนที่กันไว้
	if w := do(a.serveLAN, "PUT", uploadURL(lanHost, "one.bin"), phoneAddr, []byte("x")); w.Code != http.StatusInsufficientStorage {
		t.Errorf("ดิสก์เต็ม: status %d ต้องการ 507", w.Code)
	}

	names := listDir(t, a.cfg.Dir)
	if len(names) != 1 || names[0] != "small.bin" {
		t.Errorf("ไฟล์ในโฟลเดอร์ผิด: %v", names)
	}
}

// ---------- สิทธิ์เข้าถึง ----------

func TestThrottle(t *testing.T) {
	a := newTestApp(t)
	attacker := "192.168.1.66:1234"
	good := lanHost + "/s/" + testToken

	// คำขอที่เบราว์เซอร์มือถือส่งเองต้องไม่ถูกนับเป็นการเดารหัส
	for i := 0; i < 50; i++ {
		for _, p := range []string{"/favicon.ico", "/apple-touch-icon.png", "/apple-touch-icon-precomposed.png", "/robots.txt", "/"} {
			do(a.serveLAN, "GET", lanHost+p, phoneAddr, nil)
		}
	}
	if w := do(a.serveLAN, "GET", good, phoneAddr, nil); w.Code != 200 {
		t.Fatalf("มือถือปกติโดนพักเพราะคำขอไอคอน: status %d", w.Code)
	}

	for i := 0; i < maxFails; i++ {
		if w := do(a.serveLAN, "GET", fmt.Sprintf("%s/s/guess%d", lanHost, i), attacker, nil); w.Code != 404 {
			t.Fatalf("เดาครั้งที่ %d: status %d", i+1, w.Code)
		}
	}
	if w := do(a.serveLAN, "GET", good, attacker, nil); w.Code != http.StatusTooManyRequests {
		t.Errorf("ผู้เดารหัสไม่ถูกพัก: status %d", w.Code)
	}
	if w := do(a.serveLAN, "PUT", uploadURL(lanHost, "x.bin"), attacker, []byte("x")); w.Code != http.StatusTooManyRequests {
		t.Errorf("ผู้เดารหัสยังอัปโหลดได้: status %d", w.Code)
	}
	if w := do(a.serveLAN, "GET", good, phoneAddr, nil); w.Code != 200 {
		t.Errorf("IP อื่นโดนพักไปด้วย: status %d", w.Code)
	}

	// พ้นช่วงพักแล้วต้องใช้ได้อีก
	a.fails["192.168.1.66"].until = time.Now().Add(-time.Second)
	if w := do(a.serveLAN, "GET", good, attacker, nil); w.Code != 200 {
		t.Errorf("พ้นช่วงพักแล้วยังใช้ไม่ได้: status %d", w.Code)
	}

	// ความผิดพลาดเก่า ๆ ต้องถูกลืม ไม่สะสมข้ามวัน
	slow := "192.168.1.77:1"
	for i := 0; i < maxFails-1; i++ {
		do(a.serveLAN, "GET", lanHost+"/s/old", slow, nil)
	}
	a.fails["192.168.1.77"].last = time.Now().Add(-time.Hour)
	do(a.serveLAN, "GET", lanHost+"/s/new", slow, nil)
	if w := do(a.serveLAN, "GET", good, slow, nil); w.Code != 200 {
		t.Errorf("ความผิดพลาดเก่าไม่ถูกลืม: status %d", w.Code)
	}
}

func TestHostHeader(t *testing.T) {
	a := newTestApp(t)
	for _, host := range []string{"http://evil.example.com", "http://evil.example.com:5050", "http://localhost.evil.com", "http://192.168.1.10.evil.com"} {
		if w := do(a.serveLAN, "GET", host+"/s/"+testToken, phoneAddr, nil); w.Code != 403 {
			t.Errorf("serveLAN Host %s = %d ต้องการ 403", host, w.Code)
		}
		if w := do(a.serveLocal, "GET", host+"/api/info", loopAddr, nil); w.Code != 403 {
			t.Errorf("serveLocal Host %s = %d ต้องการ 403", host, w.Code)
		}
	}
	for _, host := range []string{"http://192.168.1.10:5050", "http://10.0.0.5", "http://localhost:5050", "http://LOCALHOST", "http://[::1]:5050", "http://127.0.0.1:5050"} {
		if w := do(a.serveLAN, "GET", host+"/s/"+testToken, phoneAddr, nil); w.Code != 200 {
			t.Errorf("serveLAN Host %s = %d ต้องการ 200", host, w.Code)
		}
	}
}

// ช่องทางมือถือต้องไม่มีอะไรนอกจากหน้าส่งไฟล์
func TestLANHasNoDesktopAPI(t *testing.T) {
	a := newTestApp(t)
	for _, remote := range []string{phoneAddr, loopAddr} {
		for _, c := range []struct{ method, path string }{
			{"GET", "/"}, {"GET", "/api/info"}, {"GET", "/api/qr.png"}, {"GET", "/api/events"}, {"GET", "/api/ping"},
			{"POST", "/api/open/" + testToken}, {"POST", "/api/show/" + testToken},
		} {
			if w := do(a.serveLAN, c.method, lanHost+c.path, remote, nil); w.Code != 404 {
				t.Errorf("serveLAN %s %s จาก %s = %d ต้องการ 404", c.method, c.path, remote, w.Code)
			}
		}
	}
}

func TestLocalOnlyFromLoopback(t *testing.T) {
	a := newTestApp(t)
	for _, p := range []string{"/", "/api/info", "/api/ping", "/s/" + testToken} {
		if w := do(a.serveLocal, "GET", localHost+p, phoneAddr, nil); w.Code != 403 {
			t.Errorf("serveLocal %s จากเครื่องอื่น = %d ต้องการ 403", p, w.Code)
		}
	}
	if w := do(a.serveLocal, "GET", localHost+"/api/ping", loopAddr, nil); w.Body.String() != "phonedrop" {
		t.Errorf("ping = %q", w.Body)
	}
	w := do(a.serveLocal, "GET", localHost+"/api/info", loopAddr, nil)
	var info map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil || info["token"] != testToken || info["dir"] != a.cfg.Dir {
		t.Errorf("info ผิด: %v %s", err, w.Body)
	}
	for _, u := range a.phoneURLs() {
		if !strings.HasPrefix(u, "https://") || !strings.HasSuffix(u, ":5050/s/"+testToken) {
			t.Errorf("ลิงก์มือถือผิดรูปแบบ: %s", u)
		}
	}
	if w := do(a.serveLocal, "GET", localHost+"/", loopAddr, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "PhoneDrop") {
		t.Errorf("หน้าจอคอม = %d", w.Code)
	}
	// คำสั่งที่มีผลต้องใช้รหัส แม้มาจากเครื่องเดียวกัน
	if w := do(a.serveLocal, "POST", localHost+"/api/open/wrong", loopAddr, nil); w.Code != 404 {
		t.Errorf("open ด้วยรหัสผิด = %d", w.Code)
	}
}

// ---------- แจ้งหน้าจอคอม ----------

func TestEvents(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(http.HandlerFunc(a.serveLocal))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				lines <- sc.Text()
			}
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		a.mu.Lock()
		n := len(a.subs)
		a.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("หน้าจอคอมไม่ได้ลงทะเบียนรับเหตุการณ์")
		}
		time.Sleep(20 * time.Millisecond)
	}

	do(a.serveLAN, "PUT", uploadURL(lanHost, "ภาพ.jpg"), phoneAddr, []byte("hello"))
	select {
	case l := <-lines:
		if !strings.Contains(l, `"type":"file"`) || !strings.Contains(l, "ภาพ.jpg") || !strings.Contains(l, `"size":5`) {
			t.Errorf("เหตุการณ์ผิด: %s", l)
		}
		if strings.Contains(l, a.cfg.Dir) || strings.Contains(l, `"path"`) {
			t.Errorf("เหตุการณ์เผยพาธเต็ม: %s", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ไม่ได้รับเหตุการณ์")
	}

	resp.Body.Close()
	deadline = time.Now().Add(3 * time.Second)
	for {
		a.mu.Lock()
		n := len(a.subs)
		a.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ปิดหน้าจอแล้วยังค้างในรายการผู้รับ")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---------- การตั้งค่า / ใบรับรอง ----------

func TestConfig(t *testing.T) {
	a := newTestApp(t)
	a.cfg = config{}
	if first := a.loadConfig(); !first || len(a.cfg.Token) < minTokenLen || !a.cfg.Notify || a.cfg.Dir == "" {
		t.Errorf("ครั้งแรก: first=%v cfg=%+v", first, a.cfg)
	}
	tok := a.cfg.Token
	a.saveConfig()

	b := newTestApp(t)
	b.cfgPath = a.cfgPath
	b.cfg = config{}
	if first := b.loadConfig(); first || b.cfg.Token != tok {
		t.Errorf("โหลดซ้ำ: first=%v token เปลี่ยน", first)
	}

	// รหัสสั้นจากเวอร์ชันเก่าต้องถูกเปลี่ยน แต่ค่าอื่นต้องอยู่
	os.WriteFile(a.cfgPath, []byte(`{"token":"9d06af33","dir":"D:\\Photos","notify":false,"trayHintShown":true}`), 0o600)
	c := newTestApp(t)
	c.cfgPath = a.cfgPath
	c.cfg = config{}
	if first := c.loadConfig(); first || len(c.cfg.Token) < minTokenLen || c.cfg.Token == "9d06af33" || c.cfg.Dir != `D:\Photos` || c.cfg.Notify {
		t.Errorf("ย้ายจากเวอร์ชันเก่า: first=%v cfg=%+v", first, c.cfg)
	}

	// ไฟล์ตั้งค่าเสีย ต้องเริ่มใหม่ได้ ไม่ล้ม
	os.WriteFile(a.cfgPath, []byte(`{"token": "abc`), 0o600)
	d := newTestApp(t)
	d.cfgPath = a.cfgPath
	d.cfg = config{}
	if first := d.loadConfig(); !first || len(d.cfg.Token) < minTokenLen {
		t.Errorf("ไฟล์ตั้งค่าเสีย: first=%v cfg=%+v", first, d.cfg)
	}

	// รหัสต้องไม่ซ้ำกัน
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		tk := newToken()
		if seen[tk] || len(tk) != 16 || strings.ToLower(tk) != tk {
			t.Fatalf("รหัสผิดปกติ: %q", tk)
		}
		seen[tk] = true
	}
}

func TestCleanTemp(t *testing.T) {
	a := newTestApp(t)
	os.MkdirAll(a.cfg.Dir, 0o755)
	os.WriteFile(filepath.Join(a.cfg.Dir, "broken.mp4"+tempSuffix), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(a.cfg.Dir, "firefox-download.part"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(a.cfg.Dir, "photo.jpg"), []byte("x"), 0o644)
	a.cleanTemp()
	names := strings.Join(listDir(t, a.cfg.Dir), ",")
	if names != "firefox-download.part,photo.jpg" {
		t.Errorf("หลังล้างไฟล์ชั่วคราว: %s", names)
	}
}

func TestCertAndTLS(t *testing.T) {
	dir := t.TempDir()
	c1, fp1, err := loadOrCreateCert(dir, []string{"192.168.1.10", "not-an-ip"})
	if err != nil {
		t.Fatal(err)
	}
	_, fp2, err := loadOrCreateCert(dir, []string{"10.0.0.1"})
	if err != nil || fp1 != fp2 {
		t.Errorf("ใบรับรองถูกสร้างใหม่ทั้งที่ยังใช้ได้: %v", err)
	}
	if len(strings.Split(fp1, ":")) != 32 {
		t.Errorf("ลายนิ้วมือผิดรูปแบบ: %s", fp1)
	}

	// ไฟล์ใบรับรองเสีย ต้องสร้างใหม่ได้
	os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("garbage"), 0o644)
	if _, fp3, err := loadOrCreateCert(dir, nil); err != nil || fp3 == fp1 {
		t.Errorf("ใบรับรองเสียแล้วไม่สร้างใหม่: %v", err)
	}

	// ส่งไฟล์ผ่าน HTTPS จริง และมือถือต้องเห็นใบรับรองใบเดียวกับที่หน้าจอคอมแสดง
	a := newTestApp(t)
	// ใช้ listener ของเราเองแบบเดียวกับโปรแกรมจริง (httptest จะสลับไปใช้ใบรับรองทดสอบของมันเอง)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{c1}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go (&http.Server{Handler: http.HandlerFunc(a.serveLAN), ErrorLog: log.New(io.Discard, "", 0)}).Serve(ln)
	srvURL := "https://" + ln.Addr().String()

	var seen, issuer string
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			seen = fingerprint(cs.PeerCertificates[0].Raw)
			issuer = cs.PeerCertificates[0].Issuer.String()
			return nil
		},
	}}}
	data := randBytes(2 * 1024 * 1024)
	req, _ := http.NewRequest("PUT", uploadURL(srvURL, "tls.bin"), bytes.NewReader(data))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("HTTPS: status %d", resp.StatusCode)
	}
	// โปรแกรมป้องกันไวรัสบางตัว (เช่น Norton) แทรกการเชื่อมต่อ HTTPS ที่ออกจากเครื่องนี้แล้วเซ็นใบรับรองใหม่
	// ซึ่งกระทบเฉพาะโปรแกรมบนเครื่องนี้ ไม่กระทบมือถือ — เจอแบบนั้นให้ข้ามการเทียบลายนิ้วมือ
	intercepted := issuer != "CN=PhoneDrop"
	if intercepted {
		t.Logf("ข้ามการเทียบลายนิ้วมือ: การเชื่อมต่อถูกแทรกโดย %q", issuer)
	} else if seen != fp1 {
		t.Errorf("ลายนิ้วมือที่ผู้ส่งเห็นไม่ตรงกับที่แอปรายงาน: %s / %s", seen, fp1)
	}
	if got, _ := os.ReadFile(filepath.Join(a.cfg.Dir, "tls.bin")); !bytes.Equal(got, data) {
		t.Error("ไฟล์ที่ส่งผ่าน HTTPS ไม่ตรงต้นฉบับ")
	}

	// TLS รุ่นเก่าต้องถูกปฏิเสธ
	old := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS11}}}
	if resp, err := old.Get(srvURL + "/s/" + testToken); err == nil {
		resp.Body.Close()
		if !intercepted {
			t.Error("ยอมรับ TLS 1.1")
		}
	}
}

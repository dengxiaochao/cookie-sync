package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
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
)

func testApplication(t *testing.T) *application {
	t.Helper()
	u, _ := url.Parse("http://nuc.bilibili.co:7070/sync-cookie")
	a := &application{syncURL: u, cookieFile: filepath.Join(t.TempDir(), ".bilicookie"), now: time.Now}
	if _, err := rand.Read(a.secret[:]); err != nil {
		t.Fatal(err)
	}
	return a
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLoginSyncAndResume(t *testing.T) {
	a := testApplication(t)
	query := "caller=dashboard&path=%2Ffoo%3Fa%3D1%26b%3D2&x=a+b&x=a%20b"
	calls := 0
	a.proxy = newDashboardProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Host != dashboardHost || r.URL.Host != dashboardHost || r.URL.Scheme != "https" {
			t.Errorf("上游地址错误: %s / %s", r.Host, r.URL)
		}
		if r.URL.RawQuery != query || r.URL.Path != loginPath {
			t.Errorf("原登录参数被修改: %s", r.URL)
		}
		if r.Header.Get("Cookie") != "dashboard_session=login" {
			t.Error("登录站点的 Cookie 未保留")
		}
		return &http.Response{StatusCode: http.StatusFound, Request: r,
			Header: http.Header{"Location": {"https://original.bilibili.co/foo"}, "Set-Cookie": {"session=new; Secure; HttpOnly"}, "Cache-Control": {"public"}},
			Body:   io.NopCloser(strings.NewReader("upstream"))}, nil
	}))
	first := httptest.NewRecorder()
	a.handleDashboard(first, httptest.NewRequest("GET", "https://"+dashboardHost+loginPath+"?"+query, nil))
	if first.Code != 302 || calls != 0 {
		t.Fatalf("应先同步: code=%d calls=%d", first.Code, calls)
	}
	syncURL, err := url.Parse(first.Header().Get("Location"))
	if err != nil || syncURL.Host != "nuc.bilibili.co:7070" || syncURL.Path != "/sync-cookie" {
		t.Fatalf("同步跳转错误: %v", syncURL)
	}
	syncReq := httptest.NewRequest("GET", syncURL.String(), nil)
	// 短 Cookie 也应正常保存，不再对凭证切片或打印凭证。
	syncReq.AddCookie(&http.Cookie{Name: "_AJSESSIONID", Value: "x"})
	synced := httptest.NewRecorder()
	a.handleSyncCookie(synced, syncReq)
	if synced.Code != 302 {
		t.Fatalf("同步失败: %d %s", synced.Code, synced.Body.String())
	}
	checkCookieFile(t, a.cookieFile, "x")
	resume := httptest.NewRequest("GET", synced.Header().Get("Location"), nil)
	resume.Header.Set("Cookie", "dashboard_session=login")
	last := httptest.NewRecorder()
	a.handleDashboard(last, resume)
	if last.Code != 302 || calls != 1 || last.Header().Get("Location") != "https://original.bilibili.co/foo" {
		t.Fatalf("未完成上游回跳: %d %v", last.Code, last.Header())
	}
	if last.Header().Get("Set-Cookie") != "session=new; Secure; HttpOnly" {
		t.Fatal("上游 Set-Cookie 被修改")
	}
	for _, rr := range []*httptest.ResponseRecorder{first, synced, last} {
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("登录跳转不应缓存")
		}
	}
}

func checkCookieFile(t *testing.T, path, value string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "_AJSESSIONID="+value+"\n" {
		t.Fatalf("Cookie 内容错误: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("Cookie 权限应为 0600，实际 %o", info.Mode().Perm())
	}
}

func TestSyncRejectsInvalidRequestsWithoutOverwriting(t *testing.T) {
	for _, name := range []string{"missing", "empty", "tampered", "expired", "empty-state", "duplicate-state", "method"} {
		t.Run(name, func(t *testing.T) {
			a := testApplication(t)
			if err := saveCookie(a.cookieFile, "previous"); err != nil {
				t.Fatal(err)
			}
			token := a.encodeState("caller=test&path=%2F")
			r := httptest.NewRequest("GET", "/sync-cookie?state="+token, nil)
			r.AddCookie(&http.Cookie{Name: "_AJSESSIONID", Value: "new"})
			switch name {
			case "missing":
				r.Header.Del("Cookie")
			case "empty":
				r.Header.Set("Cookie", "_AJSESSIONID=")
			case "tampered":
				r.URL.RawQuery = "state=x" + token
			case "expired":
				a.now = func() time.Time { return time.Now().Add(stateTTL) }
			case "empty-state":
				r.URL.RawQuery = "state="
			case "duplicate-state":
				r.URL.RawQuery += "&state=" + token
			case "method":
				r.Method = http.MethodDelete
			}
			w := httptest.NewRecorder()
			a.handleSyncCookie(w, r)
			if w.Code < 400 || w.Header().Get("Location") != "" {
				t.Fatalf("无效同步未被拒绝: %d", w.Code)
			}
			if !json.Valid(w.Body.Bytes()) {
				t.Fatal("错误响应不是 JSON")
			}
			checkCookieFile(t, a.cookieFile, "previous")
		})
	}
}

func TestManualSyncAndWriteFailure(t *testing.T) {
	a := testApplication(t)
	r := httptest.NewRequest("GET", "/sync-cookie", nil)
	r.AddCookie(&http.Cookie{Name: "_AJSESSIONID", Value: "manual"})
	r.Header.Set("Origin", "https://"+dashboardHost)
	w := httptest.NewRecorder()
	a.handleSyncCookie(w, r)
	if w.Code != 200 || !json.Valid(w.Body.Bytes()) || w.Header().Get("Location") != "" {
		t.Fatalf("手动同步失败: %s", w.Body)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "https://"+dashboardHost {
		t.Fatal("合法跨域请求失败")
	}
	checkCookieFile(t, a.cookieFile, "manual")
	r.Header.Set("Origin", "null")
	a.cookieFile = filepath.Join(t.TempDir(), "missing", "cookie")
	w = httptest.NewRecorder()
	a.handleSyncCookie(w, r)
	if w.Code != 500 || w.Header().Get("Location") != "" {
		t.Fatal("写入失败不应继续跳转")
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "null" {
		t.Fatal("未保留原手动页面的跨域兼容性")
	}
}

func TestOtherRequestsPassThrough(t *testing.T) {
	for _, path := range []string{"/api/v4/user/other%2Fpath?x=a%2Bb&x=c+d", loginPath + "?caller=test"} {
		t.Run(path, func(t *testing.T) {
			a := testApplication(t)
			a.proxy = newDashboardProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != "POST" || r.URL.RequestURI() != path || string(body) != "payload" || r.Header.Get("Authorization") != "test-auth" {
					t.Errorf("透传请求被修改: %s", r.URL)
				}
				return &http.Response{StatusCode: 201, Request: r, Header: http.Header{"X-Upstream": {"yes"}}, Body: io.NopCloser(strings.NewReader("response"))}, nil
			}))
			r := httptest.NewRequest("POST", "https://"+dashboardHost+path, strings.NewReader("payload"))
			r.Header.Set("Authorization", "test-auth")
			w := httptest.NewRecorder()
			a.handleDashboard(w, r)
			if w.Code != 201 || w.Body.String() != "response" || w.Header().Get("X-Upstream") != "yes" {
				t.Fatal("上游响应透传失败")
			}
		})
	}
}

func TestProxyFailuresAndInvalidResume(t *testing.T) {
	a := testApplication(t)
	a.proxy = newDashboardProxy(roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("连接失败") }))
	for _, tc := range []struct {
		path, hop string
		status    int
	}{
		{"/other", "", 502}, {"/other", "1", 508},
		{loginPath + "?" + resumeParam + "=invalid", "", 400},
		{loginPath + "?caller=" + strings.Repeat("x", maxQuery), "", 414},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "https://"+dashboardHost+tc.path, nil)
		r.Header.Set(proxyHop, tc.hop)
		a.handleDashboard(w, r)
		if w.Code != tc.status {
			t.Errorf("期望 %d，实际 %d", tc.status, w.Code)
		}
	}
}

func TestConcurrentCookieWritesAreAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".bilicookie")
	values := make(map[string]bool)
	for i := 0; i < 12; i++ {
		values["_AJSESSIONID="+strings.Repeat(fmt.Sprint(i), 1000)+"\n"] = true
	}
	if err := saveCookie(path, strings.Repeat("0", 1000)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			b, err := os.ReadFile(path)
			if err != nil || !values[string(b)] {
				t.Error("读到了不完整的 Cookie 文件")
				return
			}
		}
	}()
	var writers sync.WaitGroup
	for i := 0; i < 12; i++ {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			if err := saveCookie(path, strings.Repeat(fmt.Sprint(i), 1000)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	writers.Wait()
	close(done)
	wg.Wait()
	files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".bilicookie-*"))
	if err != nil || len(files) != 0 {
		t.Fatal("遗留了 Cookie 临时文件")
	}
}

func TestUpstreamTLSUsesOriginalHostname(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: dashboardHost}, DNSNames: []string{dashboardHost}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	var servers sync.WaitGroup
	t.Cleanup(servers.Wait)
	// 使用内存连接执行真实 TLS 握手，不依赖本机监听端口。
	dial := func(_ context.Context, _, _ string) (net.Conn, error) {
		client, server := net.Pipe()
		server.SetDeadline(time.Now().Add(5 * time.Second))
		servers.Add(1)
		go func() {
			defer servers.Done()
			defer server.Close()
			conn := tls.Server(server, serverTLS)
			if err := conn.Handshake(); err != nil {
				return
			}
			r, err := http.ReadRequest(bufio.NewReader(conn))
			if err != nil {
				t.Error(err)
				return
			}
			defer r.Body.Close()
			if r.Host != dashboardHost || conn.ConnectionState().ServerName != dashboardHost {
				t.Error("上游 Host 或 SNI 未保留原域名")
			}
			io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
		}()
		return client, nil
	}
	transport, err := newUpstreamTransport("127.0.0.1:443")
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	transport.DialContext = dial
	roots := x509.NewCertPool()
	parsed, _ := x509.ParseCertificate(der)
	roots.AddCert(parsed)
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	r := httptest.NewRequest("GET", "https://"+dashboardHost+"/other", nil)
	w := httptest.NewRecorder()
	newDashboardProxy(transport).ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("指定 IP 的 TLS 代理失败: %d %s", w.Code, w.Body)
	}
	// 信任根不匹配时必须失败，不能为兼容代理关闭证书校验。
	untrusted, _ := newUpstreamTransport("127.0.0.1:443")
	defer untrusted.CloseIdleConnections()
	untrusted.DialContext = dial
	untrusted.TLSClientConfig = &tls.Config{RootCAs: x509.NewCertPool()}
	w = httptest.NewRecorder()
	newDashboardProxy(untrusted).ServeHTTP(w, r)
	if w.Code != 502 {
		t.Fatal("错误地接受了不受信任的上游证书")
	}
}

func TestStatePreservesQueryAndRejectsOtherInstance(t *testing.T) {
	a := testApplication(t)
	query := "caller=x&path=%2F%3Fa%3D1+2&x=%00&x=%FF"
	state := a.encodeState(query)
	decoded, err := a.decodeState(state)
	if err != nil || decoded.Query != query {
		t.Fatal("状态未保留原始参数")
	}
	if _, err := testApplication(t).decodeState(state); err == nil {
		t.Fatal("重启后不应接受旧状态")
	}
}

func TestSaveFailureKeepsExistingTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "directory")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveCookie(path, "new"); err == nil {
		t.Fatal("替换目录应失败")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatal("写入失败破坏了原目标")
	}
}

package main

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const (
	dashboardHost = "dashboard-mng.biliapi.net"
	loginPath     = "/api/v4/user/dashboard_login"
	resumeParam   = "_cookie_sync_resume"
)

type config struct {
	addr         string
	proxyAddr    string
	syncURL      string
	certFile     string
	keyFile      string
	upstreamAddr string
	cookieFile   string
	syncTLS      bool
}

func main() {
	var cfg config
	flag.StringVar(&cfg.addr, "addr", ":7070", "Cookie 同步服务监听地址")
	flag.StringVar(&cfg.proxyAddr, "proxy-addr", ":443", "HTTPS 代理监听地址，提供证书后启用")
	flag.StringVar(&cfg.syncURL, "sync-url", "http://nuc.bilibili.co:7070/sync-cookie", "浏览器访问的 Cookie 同步地址")
	flag.StringVar(&cfg.certFile, "tls-cert", "", "PEM 格式的服务端证书文件")
	flag.StringVar(&cfg.keyFile, "tls-key", "", "PEM 格式的服务端私钥文件")
	flag.BoolVar(&cfg.syncTLS, "sync-tls", false, "同步端口也启用 HTTPS，使用同一证书")
	flag.StringVar(&cfg.upstreamAddr, "upstream-addr", "", "可选的真实上游 IP:端口，TLS 仍校验原域名")
	flag.StringVar(&cfg.cookieFile, "cookie-file", "", "Cookie 保存路径，默认为 ~/.bilicookie")
	flag.Parse()
	if err := run(cfg); err != nil {
		log.Fatal(err)
	}
}

func run(cfg config) error {
	if (cfg.certFile == "") != (cfg.keyFile == "") {
		return errors.New("tls-cert 和 tls-key 必须同时提供")
	}
	if cfg.syncTLS && cfg.certFile == "" {
		return errors.New("sync-tls 需要提供 tls-cert 和 tls-key")
	}
	u, err := url.Parse(cfg.syncURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "/sync-cookie" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("sync-url 必须是无认证信息、查询参数和片段的 HTTP(S) /sync-cookie 地址")
	}
	if cfg.syncTLS && u.Scheme != "https" {
		return errors.New("启用 sync-tls 时，sync-url 必须使用 https")
	}
	if cfg.cookieFile == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("获取 HOME 目录: %w", err)
		}
		cfg.cookieFile = filepath.Join(home, ".bilicookie")
	}
	a := &application{syncURL: u, cookieFile: cfg.cookieFile, now: time.Now}
	if _, err := rand.Read(a.secret[:]); err != nil {
		return fmt.Errorf("生成回跳签名密钥: %w", err)
	}
	transport, err := newUpstreamTransport(cfg.upstreamAddr)
	if err != nil {
		return err
	}
	defer transport.CloseIdleConnections()
	a.proxy = newDashboardProxy(transport)

	var tlsConfig *tls.Config
	if cfg.certFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.certFile, cfg.keyFile)
		if err != nil {
			return fmt.Errorf("加载 HTTPS 证书: %w", err)
		}
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	}
	syncMux := http.NewServeMux()
	syncMux.HandleFunc("/sync-cookie", a.handleSyncCookie)
	syncServer := newServer(cfg.addr, syncMux, tlsConfig)
	defer syncServer.Close()
	errs := make(chan error, 2)
	go func() {
		log.Printf("Cookie 同步服务监听 %s，TLS=%t", cfg.addr, cfg.syncTLS)
		if cfg.syncTLS {
			errs <- syncServer.ListenAndServeTLS("", "")
		} else {
			errs <- syncServer.ListenAndServe()
		}
	}()
	if tlsConfig != nil {
		proxyServer := newServer(cfg.proxyAddr, http.HandlerFunc(a.handleDashboard), tlsConfig.Clone())
		defer proxyServer.Close()
		go func() {
			log.Printf("HTTPS 代理监听 %s，目标 %s", cfg.proxyAddr, dashboardHost)
			errs <- proxyServer.ListenAndServeTLS("", "")
		}()
	} else {
		log.Print("未提供证书，仅启动手动同步服务；自动同步需要 tls-cert 和 tls-key")
	}
	return <-errs
}

func newServer(addr string, handler http.Handler, tlsConfig *tls.Config) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

type application struct {
	syncURL    *url.URL
	cookieFile string
	secret     [32]byte
	now        func() time.Time
	proxy      http.Handler
}

func (a *application) handleSyncCookie(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// 保留手动同步接口的跨域行为，自动同步通过顶层导航完成。
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Add("Vary", "Origin")
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST, OPTIONS")
		writeResult(w, http.StatusMethodNotAllowed, -1, "仅支持 GET 和 POST")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeResult(w, http.StatusBadRequest, -1, "查询参数无效")
		return
	}
	states, auto := query["state"]
	var state string
	if auto {
		if len(states) != 1 {
			writeResult(w, http.StatusBadRequest, -1, "回跳状态无效")
			return
		}
		state = states[0]
		if _, err := a.decodeState(state); err != nil {
			writeResult(w, http.StatusBadRequest, -1, "回跳状态无效或已过期，请重新打开原页面登录")
			return
		}
	}
	cookie, err := r.Cookie("_AJSESSIONID")
	if err != nil || cookie.Value == "" || cookie.Valid() != nil {
		writeResult(w, http.StatusBadRequest, -1, "未收到有效的 _AJSESSIONID，请检查 Cookie 的 Domain、Path、Secure 和 SameSite 属性")
		return
	}
	if err := saveCookie(a.cookieFile, cookie.Value); err != nil {
		log.Printf("写入 Cookie 文件失败: %v", err)
		writeResult(w, http.StatusInternalServerError, -1, "写入 Cookie 文件失败")
		return
	}
	log.Print("Cookie 文件已更新")
	if auto {
		// 只返回固定的登录回调，原始参数由签名状态恢复。
		target := url.URL{Scheme: "https", Host: dashboardHost, Path: loginPath,
			RawQuery: url.Values{resumeParam: {state}}.Encode()}
		http.Redirect(w, r, target.String(), http.StatusFound)
		return
	}
	writeResult(w, http.StatusOK, 0, "Cookie 文件已更新")
}

func writeResult(w http.ResponseWriter, status, code int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{code, message})
}

func saveCookie(path, value string) error {
	// 在同一目录写入临时文件后替换，读者只能看到完整的新旧版本。
	f, err := os.CreateTemp(filepath.Dir(path), ".bilicookie-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := fmt.Fprintf(f, "_AJSESSIONID=%s\n", value); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

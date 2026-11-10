package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	stateTTL = 5 * time.Minute
	proxyHop = "X-Cookie-Sync-Proxy"
	maxQuery = 8 << 10
	maxState = 32 << 10
)

// loginState 保存原始查询串，避免重新编码改变 caller/path 的含义。
type loginState struct {
	Query   string
	Expires int64
}

func (a *application) encodeState(query string) string {
	data := strconv.FormatInt(a.now().Add(stateTTL).Unix(), 10) + "\n" + query
	payload := base64.RawURLEncoding.EncodeToString([]byte(data))
	mac := hmac.New(sha256.New, a.secret[:])
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *application) decodeState(token string) (loginState, error) {
	var state loginState
	if len(token) > maxState {
		return state, errors.New("状态过长")
	}
	payload, signature, ok := strings.Cut(token, ".")
	if !ok {
		return state, errors.New("状态格式错误")
	}
	sig, err := base64.RawURLEncoding.DecodeString(signature)
	mac := hmac.New(sha256.New, a.secret[:])
	mac.Write([]byte(payload))
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return state, errors.New("状态签名错误")
	}
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return state, errors.New("状态内容错误")
	}
	expires, query, ok := strings.Cut(string(data), "\n")
	state.Expires, err = strconv.ParseInt(expires, 10, 64)
	state.Query = query
	if !ok || err != nil {
		return state, errors.New("状态内容错误")
	}
	if a.now().Unix() >= state.Expires || len(state.Query) > maxQuery {
		return state, errors.New("状态已过期或内容过长")
	}
	return state, nil
}

func (a *application) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(proxyHop) != "" {
		http.Error(w, "代理请求回到了自身，请检查上游 DNS 或 upstream-addr", http.StatusLoopDetected)
		return
	}
	if r.URL.Path != loginPath || r.Method != http.MethodGet {
		a.proxy.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// 回跳状态是编码后的原始查询串，允许独立的长度上限。
	if len(r.URL.RawQuery) > maxState+len(resumeParam)+1 {
		http.Error(w, "登录参数过长", http.StatusRequestURITooLong)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, "登录参数无效", http.StatusBadRequest)
		return
	}
	if values, ok := query[resumeParam]; ok {
		if len(values) != 1 {
			http.Error(w, "回跳状态无效", http.StatusBadRequest)
			return
		}
		state, err := a.decodeState(values[0])
		if err != nil {
			http.Error(w, "回跳状态无效或已过期，请重新打开原页面登录", http.StatusBadRequest)
			return
		}
		// 将原请求交给上游完成登录回跳，不重复进入同步流程。
		r = r.Clone(r.Context())
		r.URL.RawQuery = state.Query
		r.RequestURI = r.URL.RequestURI()
		a.proxy.ServeHTTP(w, r)
		return
	}
	if len(r.URL.RawQuery) > maxQuery {
		http.Error(w, "登录参数过长", http.StatusRequestURITooLong)
		return
	}
	target := *a.syncURL
	target.RawQuery = url.Values{"state": {a.encodeState(r.URL.RawQuery)}}.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func newUpstreamTransport(addr string) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// 上游使用直连，避免环境代理再次将请求送回本服务。
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 30 * time.Second
	if addr != "" {
		host, port, err := net.SplitHostPort(addr)
		portNumber, portErr := strconv.Atoi(port)
		if err != nil || net.ParseIP(host) == nil || portErr != nil || portNumber < 1 || portNumber > 65535 {
			return nil, errors.New("upstream-addr 必须是 IP:端口，端口范围为 1–65535")
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}
	return transport, nil
}

func newDashboardProxy(transport http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			// 只替换目的主机，保留方法、请求体、路径、原始查询串和业务头。
			r.Out.URL.Scheme = "https"
			r.Out.URL.Host = dashboardHost
			r.Out.URL.RawQuery = r.In.URL.RawQuery
			r.Out.Host = dashboardHost
			r.Out.Header.Set(proxyHop, "1")
		},
		ModifyResponse: func(r *http.Response) error {
			if r.Request.URL.Path == loginPath {
				r.Header.Set("Cache-Control", "no-store")
				r.Header.Set("Referrer-Policy", "no-referrer")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// 不记录请求 URL、请求头或 Cookie。
			log.Printf("登录站点上游连接失败: %v", err)
			http.Error(w, "登录站点上游暂不可用", http.StatusBadGateway)
		},
	}
}

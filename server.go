package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

func main() {
	addr := ":7070"

	http.HandleFunc("/sync-cookie", handleSyncCookie)

	log.Printf("Cookie 同步服务启动，监听 %s\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func handleSyncCookie(w http.ResponseWriter, r *http.Request) {
	// 允许跨域（方便从 dashboard-mng 等域名调用）
	origin := r.Header.Get("Origin")
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 从请求中读取 _AJSESSIONID cookie
	cookie, err := r.Cookie("_AJSESSIONID")
	if err != nil {
		http.Error(w, `{"code":-1,"message":"未找到 _AJSESSIONID cookie"}`, http.StatusBadRequest)
		return
	}

	// 写入 ~/.bilicookie
	home, err := os.UserHomeDir()
	if err != nil {
		http.Error(w, `{"code":-1,"message":"获取 HOME 目录失败"}`, http.StatusInternalServerError)
		return
	}

	cookiePath := filepath.Join(home, ".bilicookie")
	content := fmt.Sprintf("_AJSESSIONID=%s\n", cookie.Value)

	if err := os.WriteFile(cookiePath, []byte(content), 0600); err != nil {
		http.Error(w, `{"code":-1,"message":"写入文件失败"}`, http.StatusInternalServerError)
		return
	}

	log.Printf("已更新 _AJSESSIONID: %s...%s\n", cookie.Value[:4], cookie.Value[len(cookie.Value)-4:])

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"code":0,"message":"已更新 ~/.bilicookie"}`)
}

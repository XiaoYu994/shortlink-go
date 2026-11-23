package console

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

/*
newWebSPAHandler 托管前端构建产物目录（WEB_DIST_DIR，ADR 0021）。
- 只接受 GET/HEAD；
- 严格放行 /api、/r、/healthz、/readyz、/metrics 的 404 语义，绝不把不存在的 API 或短链路径回退到 index.html；
- 命中真实静态文件直接返回，否则回退 index.html 支撑前端路由。
*/
func newWebSPAHandler(distDir string) http.Handler {
	fsys := os.DirFS(distDir)
	fileServer := http.FileServerFS(fsys)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		cleanPath := path.Clean("/" + r.URL.Path)
		if isReservedBackendPath(cleanPath) {
			http.NotFound(w, r)
			return
		}

		rel := strings.TrimPrefix(cleanPath, "/")
		if rel != "" {
			if info, err := fs.Stat(fsys, rel); err == nil && !info.IsDir() {
				if strings.HasPrefix(rel, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		// 前端路由回退到 index.html，不缓存以便发布新版本后立刻生效。
		indexBytes, err := fs.ReadFile(fsys, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(indexBytes)
		}
	})
}

func isReservedBackendPath(cleanPath string) bool {
	return cleanPath == "/api" || strings.HasPrefix(cleanPath, "/api/") ||
		cleanPath == "/r" || strings.HasPrefix(cleanPath, "/r/") ||
		cleanPath == "/healthz" || cleanPath == "/readyz" || cleanPath == "/metrics"
}

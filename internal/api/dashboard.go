package api

import (
	"embed"
	"net/http"
)

//go:embed ui/index.html
var dashboardFiles embed.FS

func registerControlDashboard(mux *http.ServeMux) {
	mux.Handle("GET /", http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "text/html; charset=utf-8")
		response.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(response, request, dashboardFiles, "ui/index.html")
	}))
}

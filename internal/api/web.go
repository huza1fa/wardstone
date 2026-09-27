package api

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web/*
var webAssets embed.FS

func (s *Server) registerWeb(mux *http.ServeMux) {
	assets, err := fs.Sub(webAssets, "web")
	if err != nil {
		panic(err)
	}
	assetHandler := http.StripPrefix("/admin/assets/", http.FileServer(http.FS(assets)))
	mux.Handle("GET /admin/assets/", securityHeaders(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "public, max-age=3600")
		assetHandler.ServeHTTP(writer, request)
	})))
	mux.HandleFunc("GET /admin", func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/admin/", http.StatusMovedPermanently)
	})
	mux.Handle("GET /admin/", securityHeaders(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/admin/" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(writer, request, assets, "index.html")
	})))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(writer, request)
	})
}

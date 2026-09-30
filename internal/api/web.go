package api

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"time"
)

//go:embed web/*
var webAssets embed.FS

func (s *Server) registerWeb(mux *http.ServeMux) {
	assets, err := fs.Sub(webAssets, "web")
	if err != nil {
		panic(err)
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		panic(err)
	}
	// The HTML always refers to the exact embedded build of its CSS and JS.
	// This prevents cached assets from mixing an old console with a new API.
	for _, name := range []string{"styles.css", "app.js"} {
		content, err := fs.ReadFile(assets, name)
		if err != nil {
			panic(err)
		}
		digest := sha256.Sum256(content)
		index = bytes.ReplaceAll(index, []byte("assets/"+name), []byte(fmt.Sprintf("assets/%s?v=%x", name, digest[:8])))
	}
	assetHandler := http.StripPrefix("/admin/assets/", http.FileServer(http.FS(assets)))
	mux.Handle("GET /admin/assets/", securityHeaders(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-cache")
		if request.URL.Query().Get("v") != "" {
			writer.Header().Set("Cache-Control", "public, max-age=3600")
		}
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
		http.ServeContent(writer, request, "index.html", time.Time{}, bytes.NewReader(index))
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

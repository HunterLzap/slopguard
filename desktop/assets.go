//go:build windows || darwin

package main

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed frontend
var frontend embed.FS

func panelAssets() (http.Handler, error) {
	public, err := fs.Sub(frontend, "frontend")
	if err != nil {
		return nil, err
	}
	handler := application.BundledAssetFileServer(public)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slopguard.png" {
			w.Header().Set("Content-Type", "image/png")
			http.ServeContent(w, r, "slopguard.png", time.Time{}, bytes.NewReader(appIcon))
			return
		}
		handler.ServeHTTP(w, r)
	}), nil
}

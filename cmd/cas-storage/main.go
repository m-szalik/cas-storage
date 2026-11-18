// Command cas-storage is a thin, content-addressed file store on a local
// directory. The id of a stored object is the SHA-256 of its content.
package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/m-szalik/goutils"

	"github.com/m-szalik/cas-storage/cas"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	root := goutils.Env("STORAGE_DIR", "/data")
	addr := goutils.Env("LISTEN_ADDR", ":8080")
	// DELETE /objects/{hash} only where objects are pruned; by default every
	// object is kept.
	allowDelete := goutils.Env("STORAGE_ALLOW_DELETE", false)
	// Store text, XML, HTML and JSON objects gzipped; the objects already on
	// disk are compressed in the background at start.
	compress := goutils.Env("STORAGE_COMPRESS", false)

	// API keys and namespaces; without the file every request is denied.
	cfgPath := goutils.Env("STORAGE_CONFIG", "/etc/cas-storage/config.yaml")

	auth, err := cas.LoadConfig(cfgPath, root, compress)
	if errors.Is(err, fs.ErrNotExist) {
		log.Warn("config file not found, every request is denied", "config", cfgPath)
		auth, err = cas.Config{}.Open(root, compress)
	} else if err == nil && auth.KeyCount() == 0 {
		log.Warn("no API keys configured, every request is denied", "config", cfgPath)
	}
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           cas.Handler(auth, log, allowDelete),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if compress {
		for name, store := range auth.Namespaces {
			nsLog := log.With("namespace", name)
			go func() {
				if err := store.CompressExisting(ctx, nsLog); err != nil && ctx.Err() == nil {
					nsLog.Error("compress existing objects", "err", err)
				}
			}()
		}
	}

	go func() {
		log.Info("cas-storage listening", "addr", addr, "dir", root, "config", cfgPath,
			"namespaces", len(auth.Namespaces), "allow_delete", allowDelete, "compress", compress)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
	log.Info("cas-storage stopped")
}

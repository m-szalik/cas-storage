// Command cas-storage is a thin, content-addressed file store on a local
// directory. The id of a stored object is the SHA-256 of its content.
package main

import (
	"context"
	"errors"
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

	store, err := cas.Open(root)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	store.Compress = compress

	srv := &http.Server{
		Addr:              addr,
		Handler:           cas.Handler(store, log, allowDelete),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if compress {
		go func() {
			if err := store.CompressExisting(ctx, log); err != nil && ctx.Err() == nil {
				log.Error("compress existing objects", "err", err)
			}
		}()
	}

	go func() {
		log.Info("cas-storage listening", "addr", addr, "dir", root, "allow_delete", allowDelete, "compress", compress)
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

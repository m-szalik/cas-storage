package cas

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/m-szalik/goutils"
)

// PutResult is the JSON returned when an object is stored.
type PutResult struct {
	Hash    string `json:"hash"`
	Size    int64  `json:"size"`
	Existed bool   `json:"existed"`
}

// DeleteResult is the JSON returned by DELETE /objects/{hash}.
type DeleteResult struct {
	Size    int64 `json:"size"`
	Deleted bool  `json:"deleted"`
}

// Handler builds the cas-storage HTTP router.
//
//	POST   /objects        store bytes, returns {hash,size,existed}
//	GET    /objects/{hash}  stream object (supports Range), 404 if absent
//	HEAD   /objects/{hash}  existence + size
//	DELETE /objects/{hash}?unused_before=<RFC 3339>
//	                        remove it unless put since then (allowDelete only):
//	                        200 {size,deleted}, 404 if absent
//	GET    /healthz         liveness
func Handler(store *Store, log *slog.Logger, allowDelete bool) http.Handler {
	mux := http.NewServeMux()

	if allowDelete {
		mux.HandleFunc("DELETE /objects/{hash}", func(w http.ResponseWriter, r *http.Request) {
			before, err := time.Parse(time.RFC3339, r.URL.Query().Get("unused_before"))
			if err != nil {
				http.Error(w, "unused_before (RFC 3339) required", http.StatusBadRequest)
				return
			}
			hash := r.PathValue("hash")
			size, deleted, err := store.Delete(hash, before)
			if errors.Is(err, ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			if err != nil {
				log.Error("delete object", "hash", hash, "err", err)
				http.Error(w, "delete failed", http.StatusInternalServerError)
				return
			}
			if deleted {
				log.Info("object deleted", "hash", hash, "bytes", size)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(DeleteResult{Size: size, Deleted: deleted})
		})
	}

	mux.HandleFunc("POST /objects", func(w http.ResponseWriter, r *http.Request) {
		hash, size, existed, err := store.Put(r.Body)
		if err != nil {
			log.Error("put object", "err", err)
			http.Error(w, "store failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Location", "/objects/"+hash)
		w.Header().Set("Content-Type", "application/json")
		if existed {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusCreated)
		}
		_ = json.NewEncoder(w).Encode(PutResult{Hash: hash, Size: size, Existed: existed})
	})

	serveObject := func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("hash")
		f, fi, err := store.Open(hash)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			log.Error("open object", "hash", hash, "err", err)
			http.Error(w, "open failed", http.StatusInternalServerError)
			return
		}
		defer goutils.CloseQuietly(f)
		// ServeContent adds Range, conditional and content sniffing for free.
		http.ServeContent(w, r, hash, fi.ModTime(), f)
	}
	mux.HandleFunc("GET /objects/{hash}", serveObject)
	mux.HandleFunc("HEAD /objects/{hash}", serveObject)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})

	return withLogging(mux, log)
}

// withLogging records method, path, status and latency for each request.
func withLogging(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)
		log.Info("request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur", dur.String(), "dur_ms", dur.Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

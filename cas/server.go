package cas

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/m-szalik/goutils"
)

// maxLabelBody caps the body of PUT /objects/{hash}/labels/{key}.
const maxLabelBody = 4 << 10

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

// FindResult is the JSON returned by GET /objects?label=...
type FindResult struct {
	Hashes []string `json:"hashes"`
}

type accessCtxKey struct{}

// accessOf returns what the authenticated request's key grants.
func accessOf(r *http.Request) *access { return r.Context().Value(accessCtxKey{}).(*access) }

// storeOf returns the namespace store of the authenticated request.
func storeOf(r *http.Request) *Store { return accessOf(r).store }

// Handler builds the cas-storage HTTP router. Every route but /healthz needs
// an API key (X-API-Key or Authorization: Bearer) and works on that key's
// namespace: 401 for a missing or unknown key, 403 without the permission.
//
//	POST   /objects        store bytes, returns {hash,size,existed}       CREATE
//	GET    /objects/{hash}  stream object (supports Range), 404 if absent  READ
//	HEAD   /objects/{hash}  existence + size                               READ
//	DELETE /objects/{hash}?unused_before=<RFC 3339>                        DELETE
//	                        remove it unless put since then (allowDelete only):
//	                        200 {size,deleted}, 404 if absent
//	GET    /objects/{hash}/labels       {key:value,...}, 404 if absent     READ
//	PUT    /objects/{hash}/labels/{key} body = value, 204                  LABEL
//	DELETE /objects/{hash}/labels/{key} 204 (idempotent)                   LABEL
//	GET    /objects?label=k=v&label=k2  {hashes:[...]}, all must match     READ
//	GET    /healthz         liveness, no key
func Handler(auth *Auth, log *slog.Logger, allowDelete bool) http.Handler {
	mux := http.NewServeMux()
	handle := func(pattern string, perm Permission, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !accessOf(r).perms[perm] {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			h(w, r)
		})
	}

	if allowDelete {
		handle("DELETE /objects/{hash}", PermDelete, func(w http.ResponseWriter, r *http.Request) {
			before, err := time.Parse(time.RFC3339, r.URL.Query().Get("unused_before"))
			if err != nil {
				http.Error(w, "unused_before (RFC 3339) required", http.StatusBadRequest)
				return
			}
			hash := r.PathValue("hash")
			size, deleted, err := storeOf(r).Delete(hash, before)
			if errors.Is(err, ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			if err != nil {
				log.Error("delete object", "namespace", accessOf(r).namespace, "hash", hash, "err", err)
				http.Error(w, "delete failed", http.StatusInternalServerError)
				return
			}
			if deleted {
				log.Info("object deleted", "namespace", accessOf(r).namespace, "hash", hash, "bytes", size)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(DeleteResult{Size: size, Deleted: deleted})
		})
	}

	handle("POST /objects", PermCreate, func(w http.ResponseWriter, r *http.Request) {
		store := storeOf(r)
		body := r.Body
		if store.MaxUploadSize > 0 {
			if r.ContentLength > store.MaxUploadSize {
				http.Error(w, "object too large", http.StatusRequestEntityTooLarge)
				return
			}
			body = http.MaxBytesReader(w, r.Body, store.MaxUploadSize)
		}
		hash, size, existed, err := store.Put(body)
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "object too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			log.Error("put object", "namespace", accessOf(r).namespace, "err", err)
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
		f, fi, err := storeOf(r).Open(hash)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			log.Error("open object", "namespace", accessOf(r).namespace, "hash", hash, "err", err)
			http.Error(w, "open failed", http.StatusInternalServerError)
			return
		}
		defer goutils.CloseQuietly(f)
		// ServeContent adds Range, conditional and content sniffing for free.
		http.ServeContent(w, r, hash, fi.ModTime(), f)
	}
	handle("GET /objects/{hash}", PermRead, serveObject)
	handle("HEAD /objects/{hash}", PermRead, serveObject)

	handle("GET /objects/{hash}/labels", PermRead, func(w http.ResponseWriter, r *http.Request) {
		labels, err := storeOf(r).Labels(r.PathValue("hash"))
		if labelError(w, r, log, "read labels", err) {
			return
		}
		if labels == nil {
			labels = map[string]string{}
		}
		writeJSON(w, labels)
	})

	handle("PUT /objects/{hash}/labels/{key...}", PermLabel, func(w http.ResponseWriter, r *http.Request) {
		value, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLabelBody))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "label value too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}
		err = storeOf(r).SetLabel(r.PathValue("hash"), r.PathValue("key"), string(value))
		if labelError(w, r, log, "set label", err) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	handle("DELETE /objects/{hash}/labels/{key...}", PermLabel, func(w http.ResponseWriter, r *http.Request) {
		err := storeOf(r).DeleteLabel(r.PathValue("hash"), r.PathValue("key"))
		if labelError(w, r, log, "delete label", err) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	handle("GET /objects", PermRead, func(w http.ResponseWriter, r *http.Request) {
		params := r.URL.Query()["label"]
		if len(params) == 0 {
			http.Error(w, "at least one label=<key>[=<value>] required", http.StatusBadRequest)
			return
		}
		sel := make(map[string]string, len(params))
		for _, p := range params {
			k, v, _ := strings.Cut(p, "=")
			if old, dup := sel[k]; dup && old != v {
				http.Error(w, "conflicting selectors for label "+k, http.StatusBadRequest)
				return
			}
			sel[k] = v
		}
		hashes, err := storeOf(r).FindByLabels(sel)
		if labelError(w, r, log, "find by labels", err) {
			return
		}
		if hashes == nil {
			hashes = []string{}
		}
		writeJSON(w, FindResult{Hashes: hashes})
	})

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	root.Handle("/", withAuth(mux, auth))
	return withLogging(root, log)
}

// labelError answers err (nil: nothing) as 404, 400 or 500 and reports
// whether it did.
func labelError(w http.ResponseWriter, r *http.Request, log *slog.Logger, op string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, ErrInvalidLabel):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		log.Error(op, "namespace", accessOf(r).namespace, "path", r.URL.Path, "err", err)
		http.Error(w, op+" failed", http.StatusInternalServerError)
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// withAuth resolves the request's API key and passes its access on in the
// context; a missing or unknown key gets 401.
func withAuth(next http.Handler, auth *Auth) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "" {
			if scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
				key = strings.TrimSpace(token)
			}
		}
		acc := auth.lookup(key)
		if acc == nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if rec, ok := w.(*statusRecorder); ok {
			rec.namespace = acc.namespace
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accessCtxKey{}, acc)))
	})
}

// withLogging records method, path, namespace, status and latency for each
// request.
func withLogging(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		dur := time.Since(start)
		log.Info("request",
			"method", r.Method, "path", r.URL.Path, "namespace", rec.namespace,
			"status", rec.status, "dur", dur.String(), "dur_ms", dur.Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status    int
	namespace string
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

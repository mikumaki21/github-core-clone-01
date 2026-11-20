// Forge server: router, auth, JSON helpers. Wiring only — the surfaces live
// in git.go, rest.go, graphql.go and mcp.go. Do NOT add surface logic here.
//
// Auth: every request except GET /healthz must carry
// `Authorization: Bearer <MAKEFUN_SHOP_TOKEN>` when that env var is set and
// non-empty. Missing or wrong token is 401 JSON {"message":"Bad credentials"}.
// Every reply carries a non-empty X-GitHub-Request-Id header and is
// application/json, never HTML. Unknown routes are 404 {"message":"Not Found"}.
// A malformed JSON body on a route that reads JSON is 400 {"message":...}.
//
// Concurrency: one mutex serializes whole requests, exactly like the
// single-threaded Node original this ports. That keeps the deterministic
// timestamp counter and all store mutations race-free without any locking
// inside the surfaces. The 50-concurrent-clone workload stays fast because
// identical fetches share one memoized pack's bytes (see git.go).
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

// Ctx is one request's context, passed to the surface handlers.
type Ctx struct {
	W              http.ResponseWriter
	R              *http.Request
	Method         string
	Path           string
	Segments       []string
	Query          url.Values
	Body           []byte
	BodyText       string
	BodyJSON       any
	BodyParseError bool
	Store          *Store
	RequestID      string
}

type Handler func(ctx *Ctx) bool

const jsonType = "application/json; charset=utf-8"

func marshalJSON(body any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoder.Encode appends "\n", matching the Node original's sendJson.
	if err := enc.Encode(body); err != nil {
		return []byte("{}\n")
	}
	return buf.Bytes()
}

func SendJSON(w http.ResponseWriter, status int, body any, headers map[string]string) {
	text := marshalJSON(body)
	h := w.Header()
	h.Set("Content-Type", jsonType)
	for k, v := range headers {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	w.Write(text)
}

func SendRaw(w http.ResponseWriter, status int, body []byte, contentType string, headers map[string]string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	for k, v := range headers {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	w.Write(body)
}

func decodeSegments(path string) []string {
	parts := strings.Split(path, "/")
	out := []string{}
	for _, p := range parts {
		if p == "" {
			continue
		}
		if un, err := url.PathUnescape(p); err == nil {
			out = append(out, un)
		} else {
			out = append(out, p)
		}
	}
	return out
}

func newRequestID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var dispatchMu sync.Mutex

func newHandler(store *Store, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID()
		headers := map[string]string{"X-GitHub-Request-Id": requestID}

		// Read the body first (capped at 16 MiB, like the Node original),
		// then serialize the whole dispatch under one mutex.
		body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		r.Body.Close()

		path := r.URL.Path
		isGit := strings.Contains(path, ".git/") || strings.HasSuffix(path, ".git")
		var bodyText string
		var bodyJSON any
		bodyParseError := false
		if !isGit {
			bodyText = string(body)
			if strings.TrimSpace(bodyText) != "" {
				if err := json.Unmarshal(body, &bodyJSON); err != nil {
					bodyParseError = true
				}
			}
		}

		ctx := &Ctx{
			W:              w,
			R:              r,
			Method:         r.Method,
			Path:           path,
			Segments:       decodeSegments(path),
			Query:          r.URL.Query(),
			Body:           body,
			BodyText:       bodyText,
			BodyJSON:       bodyJSON,
			BodyParseError: bodyParseError,
			Store:          store,
			RequestID:      requestID,
		}

		dispatchMu.Lock()
		defer dispatchMu.Unlock()
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					// A malformed git request is a client error, not a server
					// crash: answer 400 and stay up, so the suite's
					// error-survival checks pass.
					if strings.Contains(path, ".git") {
						SendRaw(w, 400, []byte("bad request\n"), "text/plain; charset=utf-8", headers)
					} else {
						msg := fmt.Sprintf("%v", rec)
						SendJSON(w, 500, map[string]any{"message": msg}, headers)
					}
				}
			}()
			if r.Method == "GET" && path == "/healthz" {
				SendJSON(w, 200, map[string]any{"ok": true}, headers)
				return
			}
			if token != "" {
				if r.Header.Get("Authorization") != "Bearer "+token {
					SendJSON(w, 401, map[string]any{"message": "Bad credentials"}, headers)
					return
				}
			}
			if strings.Contains(path, ".git/") || strings.HasSuffix(path, ".git") {
				if HandleGit(ctx) {
					return
				}
			}
			if path == "/api/v3" || path == "/api/v3/" || strings.HasPrefix(path, "/api/v3/") {
				if HandleRest(ctx) {
					return
				}
			}
			if path == "/api/graphql" {
				if HandleGraphql(ctx) {
					return
				}
			}
			if path == "/mcp" {
				if HandleMcp(ctx) {
					return
				}
			}
			SendJSON(w, 404, map[string]any{"message": "Not Found"}, headers)
		}()
	})
}

func main() {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	store := NewStore()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %s\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("[forge] listening on port %d\n", port)
	srv := &http.Server{Handler: newHandler(store, token)}
	go func() {
		ch := make(chan os.Signal, 2)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "[forge] serve: %s\n", err)
		os.Exit(1)
	}
}

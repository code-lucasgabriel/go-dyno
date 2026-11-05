package dyno

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Handlers that type-assert http.Flusher must keep working behind Middleware.
func TestMiddlewareFlush(t *testing.T) {
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
		w.(http.Flusher).Flush()
	}))
	rec := httptest.NewRecorder()
	before := requests.Load()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !rec.Flushed {
		t.Error("flush did not reach the underlying writer")
	}
	if requests.Load() != before+1 {
		t.Error("request not counted")
	}
}

// Hijack reaches the underlying writer, and reports its error when it can't.
func TestMiddlewareHijack(t *testing.T) {
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := w.(http.Hijacker).Hijack(); err == nil {
			t.Error("recorder can't hijack; want an error")
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

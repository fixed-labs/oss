package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDoDecodesAndSendsBearer(t *testing.T) {
	var auth, ct string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		ct = r.Header.Get("Content-Type")
		_ = json.NewEncoder(w).Encode(map[string]string{"k": "v"})
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	var out struct {
		K string `json:"k"`
	}
	if err := c.Do(context.Background(), http.MethodPost, "/x", map[string]string{"a": "b"}, &out); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if out.K != "v" {
		t.Fatalf("decode: %+v", out)
	}
	if auth != "Bearer tok" {
		t.Fatalf("auth: %q", auth)
	}
	if ct != "application/json" {
		t.Fatalf("content-type on a body request: %q", ct)
	}
}

func TestDoNon2xxIsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	err := c.Do(context.Background(), http.MethodGet, "/x", nil, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict || ae.Body != "nope" {
		t.Fatalf("want 409 APIError body 'nope', got %v", err)
	}
	if ae.Error() != "HTTP 409: nope" {
		t.Fatalf("Error(): %q", ae.Error())
	}
}

func TestDoNoBearerWhenTokenEmpty(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	_ = c.Do(context.Background(), http.MethodGet, "/x", nil, nil)
	if auth != "" {
		t.Fatalf("empty token must send no bearer, got %q", auth)
	}
}

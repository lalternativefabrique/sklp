package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDo_BearerHeaderSetWhenTokenPresent(t *testing.T) {
	var gotAuth, gotContentType, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "sklp_pat_xxx")
	_, _, err := c.Get(context.Background(), "/api/me")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotAuth != "Bearer sklp_pat_xxx" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotContentType != "" {
		t.Fatalf("no body should mean no content-type, got %q", gotContentType)
	}
	if gotAccept != "application/json" {
		t.Fatalf("accept = %q", gotAccept)
	}
}

func TestDo_NoTokenNoAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, _, err := c.Get(context.Background(), "/api/x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("auth header should be absent, got %q", gotAuth)
	}
}

func TestDo_BodySetsContentType(t *testing.T) {
	var gotContentType string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL, "tk")
	_, _, err := c.Post(context.Background(), "/x", map[string]string{"a": "b"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type = %q", gotContentType)
	}
	if gotBody["a"] != "b" {
		t.Fatalf("body = %+v", gotBody)
	}
}

func TestDo_NonSuccessReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tk")
	body, status, err := c.Get(context.Background(), "/x")
	if status != 401 {
		t.Fatalf("status = %d", status)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 401 {
		t.Fatalf("apiErr.StatusCode = %d", apiErr.StatusCode)
	}
	if string(body) != `{"error":"unauthorized"}` {
		t.Fatalf("body = %s", body)
	}
}

func TestDo_TimeoutRespected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL, "tk")
	c.HTTP = &http.Client{Timeout: 30 * time.Millisecond}
	_, _, err := c.Get(context.Background(), "/x")
	if err == nil {
		t.Fatalf("expected timeout error")
	}
}

package control

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sync/atomic"
)

// tokenSource is the token the listener authenticates against, read from its
// file on every check so a rotation by another process (`sklet token rotate`)
// takes effect without a restart. path == "" pins the token for good (tests).
type tokenSource struct {
	path string
	cur  atomic.Value
}

func newTokenSource(path, initial string) *tokenSource {
	s := &tokenSource{path: path}
	s.cur.Store(initial)
	return s
}

func (s *tokenSource) current() string { return s.cur.Load().(string) }

// matches reads the file on every call: a token rotated by another process
// must stop working at once, and a cached hit would keep honouring it.
func (s *tokenSource) matches(got string) bool {
	want := s.current()
	if s.path != "" {
		if tok, err := readToken(s.path); err == nil {
			want = tok
			s.cur.Store(tok)
		}
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *tokenSource) rotate() (string, error) {
	tok, err := generateToken()
	if err != nil {
		return "", err
	}
	if s.path != "" {
		if err := writeTokenFile(s.path, tok); err != nil {
			return "", err
		}
	}
	s.cur.Store(tok)
	return tok, nil
}

// tokenAuthSource is tokenAuth against a source that can change underneath it.
func tokenAuthSource(src *tokenSource, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !src.matches(bearerToken(r.Header.Get("Authorization"))) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sklp deploy"`)
			http.Error(w, "unauthorized: a valid Bearer token is required", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// serveTokenRotate answers POST RouteTokenRotate: the caller proves it holds
// the current token, gets the next one exactly once, and the current one dies
// with this response.
func serveTokenRotate(w http.ResponseWriter, r *http.Request, src *tokenSource) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if src == nil || src.path == "" {
		http.Error(w, "token rotation is not available on this listener", http.StatusNotImplemented)
		return
	}
	tok, err := src.rotate()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(TokenRotateResponse{Token: tok})
}

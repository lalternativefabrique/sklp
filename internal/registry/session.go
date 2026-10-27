package registry

import (
	"strings"
	"sync"
)

// The session store holds the credentials a run resolved, for the push path
// to read back. It exists because the stage that resolves credentials and the
// stage that pushes are far apart, and the alternative — writing them to a
// file both can open — is what let a secret outlive the run that needed it.
//
// Nothing here is written to disk. A credential lives for the lifetime of the
// process and no longer.
var session struct {
	mu    sync.RWMutex
	creds map[string]Resolved
}

// Remember records a credential for the rest of this process's life.
func Remember(host, username, password string) {
	host = strings.TrimSpace(host)
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.creds == nil {
		session.creds = map[string]Resolved{}
	}
	session.creds[host] = Resolved{Host: host, Username: username, Password: password}
}

// Remembered returns a credential recorded earlier in this process, if any.
func Remembered(host string) (Resolved, bool) {
	session.mu.RLock()
	defer session.mu.RUnlock()
	r, ok := session.creds[strings.TrimSpace(host)]
	return r, ok
}

// ForgetAll drops every remembered credential. Tests use it to isolate cases.
func ForgetAll() {
	session.mu.Lock()
	session.creds = nil
	session.mu.Unlock()
}

package spacemanager

import "errors"

// ErrConfig marks a failure that restarting cannot fix: the manifest asks for
// something this node cannot provide (no data disk for a named volume, an
// unresolvable declaration). It is the opposite of the transient failures
// Restart=on-failure exists for — a database not up yet, a network coming back.
//
// The supervisor turns it into a distinct exit code the unit refuses to restart
// on, so the service lands in `failed` with the reason in `systemctl status`
// rather than relaunching every few seconds forever against an error only a
// human can clear.
var ErrConfig = errors.New("configuration error")

// ConfigError wraps err as a configuration failure.
func ConfigError(err error) error {
	if err == nil {
		return nil
	}
	return configError{err}
}

type configError struct{ err error }

func (e configError) Error() string { return e.err.Error() }
func (e configError) Unwrap() error { return e.err }
func (e configError) Is(target error) bool {
	return target == ErrConfig
}

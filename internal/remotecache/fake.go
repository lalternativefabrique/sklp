package remotecache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
)

// Fake is an in-memory RemoteCache for tests. Safe for concurrent use.
// It also exposes FailGet / FailPut hooks so tests can simulate network
// failure modes deterministically.
type Fake struct {
	mu      sync.Mutex
	cas     map[string][]byte
	results map[string][]byte
	stdout  map[string][]byte
	stderr  map[string][]byte

	FailGetCAS    bool
	FailPutCAS    bool
	FailGetResult bool
	FailPutResult bool

	putCASCount    int
	putResultCount int
}

// NewFake returns a fresh empty fake.
func NewFake() *Fake {
	return &Fake{
		cas:     map[string][]byte{},
		results: map[string][]byte{},
		stdout:  map[string][]byte{},
		stderr:  map[string][]byte{},
	}
}

func (f *Fake) Status() RemoteStatus {
	return RemoteStatus{Endpoint: "fake://", Bucket: "fake"}
}

func (f *Fake) GetCAS(_ context.Context, sha string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailGetCAS {
		return nil, errors.New("fake: simulated GetCAS failure")
	}
	b, ok := f.cas[sha]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *Fake) HasCAS(_ context.Context, sha string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.cas[sha]
	return ok, nil
}

func (f *Fake) PutCAS(_ context.Context, sha string, r io.Reader, _ int64) error {
	if f.FailPutCAS {
		return errors.New("fake: simulated PutCAS failure")
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.cas[sha]; ok {
		return nil
	}
	f.cas[sha] = b
	f.putCASCount++
	return nil
}

func (f *Fake) GetResult(_ context.Context, stepKey string) (*StepResultBlob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailGetResult {
		return nil, errors.New("fake: simulated GetResult failure")
	}
	raw, ok := f.results[stepKey]
	if !ok {
		return nil, ErrNotFound
	}
	var blob StepResultBlob
	if err := jsonUnmarshal(raw, &blob); err != nil {
		return nil, err
	}
	blob.Stdout = append([]byte(nil), f.stdout[stepKey]...)
	blob.Stderr = append([]byte(nil), f.stderr[stepKey]...)
	return &blob, nil
}

func (f *Fake) PutResult(_ context.Context, stepKey string, blob *StepResultBlob) error {
	if f.FailPutResult {
		return errors.New("fake: simulated PutResult failure")
	}
	raw, err := jsonMarshal(blob)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stdout[stepKey] = append([]byte(nil), blob.Stdout...)
	f.stderr[stepKey] = append([]byte(nil), blob.Stderr...)
	f.results[stepKey] = raw
	f.putResultCount++
	return nil
}

// PutCASCount returns the number of distinct PutCAS operations that
// actually persisted bytes (idempotent re-puts are not counted).
func (f *Fake) PutCASCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putCASCount
}

// PutResultCount returns the number of PutResult calls observed.
func (f *Fake) PutResultCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putResultCount
}

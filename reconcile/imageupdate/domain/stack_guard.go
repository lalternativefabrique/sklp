package domain

import (
	"context"
	"errors"
)

// ErrForbidden is returned when the caller does not own the target stack. It
// lives in domain so every use case reports the same refusal, and none has to
// import a sibling application package for it.
var ErrForbidden = errors.New("stack not found or not yours")

// StackOwnership is the minimum view of the stack bounded context this context
// needs. Keeping it local avoids a hard dependency on the stack domain package.
type StackOwnership interface {
	IsOwner(ctx context.Context, stackID, userID string) (bool, error)
}

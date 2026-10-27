package domain

import (
	"context"
	"time"
)

// Repository is the persistence contract for the image-update bounded context.
//
// Credential fields (NodeToken, RegistrySecret) cross this interface in CLEAR:
// sealing them is the implementation's job, so a caller never has to remember to
// encrypt and a plaintext value never reaches a row by omission.
type Repository interface {
	Save(ctx context.Context, w *Watch) error
	Update(ctx context.Context, w *Watch) error
	FindByID(ctx context.Context, id string) (*Watch, error)
	FindByStackID(ctx context.Context, stackID string) ([]*Watch, error)
	Delete(ctx context.Context, id string) error

	// FindDue returns every enabled watch, for the poller to check. There is no
	// per-watch interval yet: the poller's own tick is the interval, and adding
	// one before a user asks would be a column nothing reads.
	FindDue(ctx context.Context) ([]*Watch, error)

	// ClaimRoll atomically moves a watch from idle to rolling for toTag, and
	// reports whether THIS caller won the claim. It is the dedup that a queue
	// group cannot provide: two replicas may each publish a roll for the same
	// new tag within one tick, producing two distinct messages that the group
	// happily hands to two subscribers. The row is the arbiter — only the
	// winner POSTs to the node.
	//
	// A watch already rolling toTag, or already running it, loses the claim.
	ClaimRoll(ctx context.Context, watchID, toTag string) (bool, error)

	// FinishRoll records a terminal roll outcome: it clears the rolling status,
	// stamps the tag when the roll succeeded, and appends a row to the history.
	FinishRoll(ctx context.Context, watchID string, roll *Roll) error

	// TouchChecked stamps last_checked_at and the last check error (empty on
	// success), without disturbing roll state.
	TouchChecked(ctx context.Context, watchID string, at time.Time, checkErr string) error

	// SettleRoll records the outcome the node reported for an applied roll:
	// `live` advances the tag and clears any hold, `rolled_back` holds the tag
	// so it is not retried, anything else leaves the watch on its previous tag.
	SettleRoll(ctx context.Context, watchID string, roll *Roll, outcome RollOutcome) error

	// ListPendingRolls returns every applied roll created after since whose
	// outcome has not been recorded, oldest first — what a restarted core must
	// resume watching.
	ListPendingRolls(ctx context.Context, since time.Time) ([]*Roll, error)

	// SetPinnedTag records the tag an operator rolled by hand; empty clears it.
	SetPinnedTag(ctx context.Context, watchID, tag string) error

	AdoptCurrentTag(ctx context.Context, watchID, tag string) error

	// Release clears a watch's held and pinned tags and returns it to idle.
	Release(ctx context.Context, watchID string) error

	// ListRolls returns a watch's roll history, newest first.
	ListRolls(ctx context.Context, watchID string, limit int) ([]*Roll, error)

	// ResetOrphans flips every row left in `rolling` back to idle. Called once at
	// boot: a row still rolling then was left by a replica that died mid-roll,
	// and would otherwise block that watch forever, since ClaimRoll refuses a
	// watch already rolling. Returns the number of rows recovered.
	ResetOrphans(ctx context.Context, reason string) (int, error)
}

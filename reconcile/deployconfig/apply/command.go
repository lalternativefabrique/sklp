package apply

// Command carries the pushed commit's coordinates — the same shape as
// reconcile_from_git.Command (the server's imageupdate application
// reconcile_from_git/command.go), since both handlers react to the same
// GitHub push webhook. ProjectID/UserID are what reconcile_from_git's
// Command does NOT need (a Watch has no secrets to resolve — see its
// package doc) but this one does, for a ${VAR} in a service's env: see
// issue 206ec5bd's step 4 and contract.ConfigReconciler's doc on why the
// shared interface carries them even though imageupdate ignores them.
type Command struct {
	StackID   string
	ProjectID string
	UserID    string
	Repo      string // "owner/name"
	Ref       string // the pushed commit SHA
	GitToken  string
}

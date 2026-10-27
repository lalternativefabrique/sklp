package sync

// Command reconciles a stack's image watches against the
// .sklp/<stack>/image-watches.yaml file at a given commit.
type Command struct {
	StackID  string
	Repo     string
	Ref      string
	GitToken string
}

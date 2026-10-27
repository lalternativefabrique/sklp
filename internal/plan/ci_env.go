package plan

import "os"

// InCI reports a CI context: SKLP_CI, or the conventional CI=true.
func InCI() bool {
	return os.Getenv("SKLP_CI") != "" || os.Getenv("CI") == "true" || os.Getenv("CI") == "1"
}

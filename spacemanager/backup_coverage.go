package spacemanager

import "strings"

// UnbackedVolumes names every named volume whose service declares no `backup:`,
// as "<service>:<volume>".
//
// Host binds are excluded: the operator chose that location and may already back
// it up from outside sklp. Named volumes are the ones sklp materialises, so they
// are the ones it can speak for.
//
// It exists to REPORT, never to refuse: a space with no backup still runs
// correctly, and a node that rejected one would block a deployment over a risk
// its operator may have weighed and accepted. That differs from the data-root
// check, which refuses because there the data is already somewhere that will be
// deleted.
func UnbackedVolumes(services []Service) []string {
	var out []string
	for _, svc := range services {
		if svc.Backup != nil {
			continue
		}
		for _, v := range svc.Volumes {
			if isNamedVolume(v) && !disposable([]string{volumeName(v)}) {
				out = append(out, svc.Name+":"+volumeName(v))
			}
		}
	}
	return out
}

// isNamedVolume reports whether a `volumes:` entry is a NAMED volume rather than
// a host bind ("/host/path:/ctr").
func isNamedVolume(entry string) bool {
	return !strings.HasPrefix(entry, "/") && !strings.HasPrefix(entry, "./") && !strings.HasPrefix(entry, "~")
}

// volumeName is the NAME half of a "name:/container/path" entry — what an
// operator writes in a backup command, not the container's mount point.
func volumeName(entry string) string {
	name, _, found := strings.Cut(entry, ":")
	if !found {
		return entry
	}
	return name
}

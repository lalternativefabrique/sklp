package spacemanager

import "github.com/lalternativefabrique/sklp/spacemanager/deployvars"

func LoginFileLookup(hostByVar map[string]string, base func(string) (string, bool)) func(string) (string, bool) {
	return deployvars.LoginFileLookup(hostByVar, base)
}

func hostByPasswordVar(varByHost map[string]string) map[string]string {
	return deployvars.HostByPasswordVar(varByHost)
}

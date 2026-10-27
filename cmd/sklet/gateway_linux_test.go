//go:build linux

package main

import "testing"

// The deploy path writes a systemd unit running `<binary> gateway mux`, and on a
// deploy node that binary is sklet. A verb only sklp carried made that unit exit
// 2/INVALIDARGUMENT on every restart: the node came up with its services
// running, its routes declared, and nothing listening on the front door — with
// no error anywhere in the deploy log.
func TestSkletHasTheVerbItsOwnMuxUnitRuns(t *testing.T) {
	root := rootCommand()
	var gw, muxCmd bool
	for _, c := range root.Sub {
		if c.Name != "gateway" {
			continue
		}
		gw = true
		for _, s := range c.Sub {
			if s.Name == "mux" {
				muxCmd = true
			}
		}
	}
	if !gw {
		t.Fatal("sklet has no `gateway` verb, so the mux unit it writes cannot run")
	}
	if !muxCmd {
		t.Error("sklet has `gateway` but no `mux` subcommand")
	}
}

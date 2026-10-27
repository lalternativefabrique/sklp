package netctrl

import (
	"reflect"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

func TestPassthroughPorts(t *testing.T) {
	routes := []spacemanager.ResolvedRoute{
		{Service: "livekit", Host: "meet.chat.eco"},
		{Service: "livekit", Host: "turn.chat.eco", Passthrough: 5349},
		{Service: "livekit", Host: "turn2.chat.eco", Passthrough: 5349},
		{Service: "livekit", Host: "tls.chat.eco", Passthrough: 7880},
		{Service: "other", Host: "x.chat.eco", Passthrough: 6000},
	}
	got := passthroughPorts("livekit", 7880, routes)
	if want := []int{5349}; !reflect.DeepEqual(got, want) {
		t.Fatalf("passthroughPorts = %v, want %v", got, want)
	}
}

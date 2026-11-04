package spacemanager

import (
	"strings"
	"testing"
)

const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100000A:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 0000000000000000 100 0 0 10 0
   1: 0100000A:1F90 0200000A:C350 01 00000000:00000000 00:00000000 00000000     0        0 2 0000000000000000 20 4 30 10 -1
   2: 0100000A:1F90 0200000A:C351 01 00000000:00000000 00:00000000 00000000     0        0 3 0000000000000000 20 4 30 10 -1
   3: 0100000A:1F90 0200000A:C352 08 00000000:00000000 00:00000000 00000000     0        0 4 0000000000000000 20 4 30 10 -1
   4: 0100000A:0050 0200000A:C353 01 00000000:00000000 00:00000000 00000000     0        0 5 0000000000000000 20 4 30 10 -1
   5: 0300000A:1F90 0200000A:C354 01 00000000:00000000 00:00000000 00000000     0        0 6 0000000000000000 20 4 30 10 -1
`

const procNetTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0000000000000000FFFF00000100000A:1F90 0000000000000000FFFF00000200000A:C355 01 00000000:00000000 00:00000000 00000000     0        0 7 0000000000000000 20 4 30 10 -1
   1: 00000000000000000000000001000000:1F90 00000000000000000000000001000000:C356 01 00000000:00000000 00:00000000 00000000     0        0 8 0000000000000000 20 4 30 10 -1
`

func TestCountEstablished_OnlyEstablishedToTheGivenEndpoint(t *testing.T) {
	n, err := countEstablished(strings.NewReader(procNetTCP), "10.0.0.1", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("got %d established to 10.0.0.1:8080, want 2 (listen, close-wait, other port and other ip excluded)", n)
	}
}

func TestCountEstablished_IPv4MappedInTCP6(t *testing.T) {
	n, err := countEstablished(strings.NewReader(procNetTCP6), "10.0.0.1", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("got %d, want 1 (the ::1 socket is not 10.0.0.1)", n)
	}
	n, err = countEstablished(strings.NewReader(procNetTCP6), "::1", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("got %d established to [::1]:8080, want 1", n)
	}
}

func TestCountEstablished_RejectsBadIP(t *testing.T) {
	if _, err := countEstablished(strings.NewReader(procNetTCP), "nope", 1); err == nil {
		t.Fatal("expected an error for an unparsable ip")
	}
}

func TestEstablishedConns_VanishedProcessHasNone(t *testing.T) {
	n, err := establishedConns(1<<30, "127.0.0.1", 1)
	if err != nil || n != 0 {
		t.Fatalf("got %d, %v; a process that is gone holds no connection", n, err)
	}
}

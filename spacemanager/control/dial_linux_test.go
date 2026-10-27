//go:build linux

package control

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

func echoServer(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					c.Write([]byte("echo:" + line))
				}
			}()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func TestDialServiceRelaysTheInstancePort(t *testing.T) {
	ip, port := echoServer(t)
	sup := &fakeSupervisor{services: []spacemanager.ServiceSnapshot{{
		Name:  "postgres",
		State: spacemanager.Ready,
		Instances: []spacemanager.InstanceSnapshot{
			{ID: "postgres-0", State: spacemanager.Ready, IP: ip, Port: port},
		},
	}}}
	c, cancel := serveFake(t, "shop-production", sup)
	defer cancel()

	conn, err := c.DialService(context.Background(), "postgres")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	for _, msg := range []string{"hello\n", "again\n"} {
		if _, err := conn.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		reply, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil || reply != "echo:"+msg {
			t.Fatalf("reply = %q, %v", reply, err)
		}
	}
}

func TestDialServicePrefersTheHostBackend(t *testing.T) {
	ip, port := echoServer(t)
	sup := &fakeSupervisor{
		backends: map[string]string{"postgres": net.JoinHostPort(ip, fmt.Sprint(port))},
		services: []spacemanager.ServiceSnapshot{{
			Name:  "postgres",
			State: spacemanager.Ready,
			Instances: []spacemanager.InstanceSnapshot{
				{ID: "postgres-0", State: spacemanager.Ready, IP: "10.88.0.2", Port: 5432},
			},
		}},
	}
	c, cancel := serveFake(t, "shop-production", sup)
	defer cancel()

	conn, err := c.DialService(context.Background(), "postgres")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || reply != "echo:hi\n" {
		t.Fatalf("reply = %q, %v", reply, err)
	}
}

func TestDialServiceRefusesWhatIsNotAReadyPort(t *testing.T) {
	sup := &fakeSupervisor{services: []spacemanager.ServiceSnapshot{
		{Name: "starting", State: spacemanager.Starting, Instances: []spacemanager.InstanceSnapshot{{ID: "starting-0", State: spacemanager.Starting, IP: "127.0.0.1", Port: 1}}},
		{Name: "noport", State: spacemanager.Ready, Instances: []spacemanager.InstanceSnapshot{{ID: "noport-0", State: spacemanager.Ready, IP: "127.0.0.1"}}},
	}}
	c, cancel := serveFake(t, "shop-production", sup)
	defer cancel()
	for _, svc := range []string{"starting", "noport", "absent"} {
		conn, err := c.DialService(context.Background(), svc)
		if err == nil {
			conn.Close()
			t.Fatalf("%s: a stream was opened", svc)
		}
		if !strings.Contains(err.Error(), "status 404") {
			t.Fatalf("%s: err = %v", svc, err)
		}
	}
}

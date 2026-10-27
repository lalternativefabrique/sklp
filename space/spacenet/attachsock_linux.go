//go:build linux

package spacenet

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// envAttachSock carries the filesystem path of the worker's attach control
// socket. The worker listens on it; the OCI createRuntime hook (a separate
// re-exec of the host binary) connects to it to push a veth into a container
// netns BEFORE the container's command execs. The worker shares the host mount
// namespace (it is cloned with CLONE_NEWUSER|CLONE_NEWNET only), so a
// filesystem path is reachable from both the worker and the hook process.
const envAttachSock = "SKLP_SPACE_ATTACH_SOCK"

// hookAttachRequest is the createRuntime hook's ask: attach the container whose
// init pid is Pid onto the space bridge using the host-preallocated IP/veth
// names, and register Name->IP in the space DNS. Every field except Pid is
// fixed at bundle time (the host owns IPAM and veth naming); the hook fills Pid
// from the OCI container state it reads on stdin.
type hookAttachRequest struct {
	Pid    int    `json:"pid"`
	Name   string `json:"name"`
	IfName string `json:"if"`
	CIDR   string `json:"cidr"`
	GwIP   string `json:"gw"`
	Host   string `json:"host"`
	Peer   string `json:"peer"`
}

// listenAttachServer runs in the worker: it listens on the attach socket and,
// for each connection, decodes one hookAttachRequest and routes it through reqs
// so the attach executes on the worker's single netns-pinned thread (serialised
// with the stdin command loop — both mutate the space netns). Listening is
// synchronous so the worker only reports ready once hooks can connect.
func listenAttachServer(reqs chan<- workerReq) (net.Listener, error) {
	path := os.Getenv(envAttachSock)
	if path == "" {
		return nil, nil
	}
	// A stale socket from a crashed prior worker would block Listen; the path is
	// space-private, so removing it is safe.
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("attach socket %q: %w", path, err)
	}
	// Go unlinks a listener's path on Close. The worker of a restarted agent
	// may outlive it by seconds and close after the new worker bound the same
	// path, taking the live socket with it: every createRuntime hook of the
	// space then fails to dial. The next worker removes a stale path instead.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed: worker exiting
			}
			// An attach mutates the space netns, so only a peer running as us may
			// ask for one. A non-unix conn cannot be vouched for either.
			uc, ok := conn.(*net.UnixConn)
			if !ok {
				_ = conn.Close()
				continue
			}
			if cerr := checkPeerUID(uc); cerr != nil {
				fmt.Fprintf(os.Stderr, "spacenet: attach: rejected peer: %v\n", cerr)
				_ = conn.Close()
				continue
			}
			go serveAttachConn(conn, reqs)
		}
	}()
	return ln, nil
}

// serveAttachConn handles one hook connection: decode the request, run the
// attach on the worker's pinned thread (via reqs), and reply with the result.
func serveAttachConn(conn net.Conn, reqs chan<- workerReq) {
	defer conn.Close()
	var req hookAttachRequest
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&req); err != nil {
		writeAttachReply(conn, reply{Error: fmt.Sprintf("decode attach request: %v", err)})
		return
	}
	resp := make(chan reply, 1)
	reqs <- workerReq{attach: &req, resp: resp}
	writeAttachReply(conn, <-resp)
}

// writeAttachReply sends one JSON reply on the hook connection.
func writeAttachReply(conn net.Conn, r reply) {
	b, _ := json.Marshal(r)
	b = append(b, '\n')
	_, _ = conn.Write(b)
}

// runHookAttach executes the createRuntime half of hook-driven attach on the
// worker's netns-pinned thread: move the veth peer into the container netns and
// register DNS. The createContainer hook configures that peer from inside the
// netns afterwards, before the service command execs.
func runHookAttach(names *nameTable, req *hookAttachRequest) reply {
	ifName := req.IfName
	if ifName == "" {
		ifName = "eth0"
	}
	cmd := command{
		Op:   "attach",
		PID:  req.Pid,
		IF:   ifName,
		CIDR: req.CIDR,
		Host: req.Host,
		Peer: req.Peer,
		GwIP: req.GwIP,
	}
	if err := doAttachMove(cmd); err != nil {
		return reply{Error: err.Error()}
	}
	if req.Name != "" {
		ip, _, perr := net.ParseCIDR(req.CIDR)
		if perr != nil {
			return reply{Error: fmt.Sprintf("parse attach cidr %q: %v", req.CIDR, perr)}
		}
		names.register(req.Name, ip)
	}
	return reply{OK: true}
}

// ----- createRuntime hook side (a separate re-exec of the host binary) -----

// hookEnv marks a re-exec'd process as one of the paired OCI network hooks,
// plus the fields of the attach plan. The move hook reads the container pid
// from OCI state; the configure hook already runs inside the target netns.
const (
	hookEnv          = "SKLP_SPACE_HOOK"
	hookEnvName      = "SKLP_SPACE_HOOK_NAME"
	hookEnvIf        = "SKLP_SPACE_HOOK_IF"
	hookEnvCIDR      = "SKLP_SPACE_HOOK_CIDR"
	hookEnvGw        = "SKLP_SPACE_HOOK_GW"
	hookEnvHost      = "SKLP_SPACE_HOOK_HOST"
	hookEnvPeer      = "SKLP_SPACE_HOOK_PEER"
	hookEnvSock      = "SKLP_SPACE_HOOK_SOCK"
	hookEnvAllowHost = "SKLP_SPACE_HOOK_ALLOW_HOST" // JSON []string
)

const (
	hookModeMove      = "move"
	hookModeConfigure = "configure"
)

// HookMoveEnv renders an AttachPlan for the createRuntime hook. The container
// pid is unknown at bundle time; that hook reads it from OCI state on stdin.
func HookMoveEnv(p *AttachPlan) []string {
	return hookEnvForPlan(p, hookModeMove)
}

// HookConfigureEnv renders an AttachPlan for the createContainer hook, which
// runs inside the container netns and configures the moved peer before exec.
func HookConfigureEnv(p *AttachPlan) []string {
	return hookEnvForPlan(p, hookModeConfigure)
}

func hookEnvForPlan(p *AttachPlan, mode string) []string {
	allowHostJSON, _ := json.Marshal(p.AllowHost)
	return []string{
		hookEnv + "=" + mode,
		hookEnvName + "=" + p.Name,
		hookEnvIf + "=" + p.IfName,
		hookEnvCIDR + "=" + p.CIDR,
		hookEnvGw + "=" + p.GwIP,
		hookEnvHost + "=" + p.Host,
		hookEnvPeer + "=" + p.Peer,
		hookEnvSock + "=" + p.SockPath,
		hookEnvAllowHost + "=" + string(allowHostJSON),
	}
}

// ociState is the subset of the OCI container state runc pipes to a hook on
// stdin. Only the init pid is needed: it is the attach target, and it is
// GUARANTEED alive — runc has created the container's namespaces and is blocked
// waiting for the createRuntime hooks to return before it execs the command.
type ociState struct {
	Pid int `json:"pid"`
}

// MaybeRunAttachHook turns the current process into the createRuntime attach
// hook when the hook env marker is set, then exits — it never returns in that
// case. In a normal process it is a no-op. Hosts call it from main() (alongside
// MaybeRunWorker) so the re-exec'd hook binary takes over before any other work.
//
// The hook reads the OCI container state (the init pid) on stdin, dials the
// space worker's attach socket, and asks it to push the veth into the
// container netns + register the DNS name. It exits 0 on success and non-zero on
// failure — a non-zero exit fails `runc create`, so a container whose network
// could not be wired never runs (clean: no service starts unreachable).
func MaybeRunAttachHook() {
	switch os.Getenv(hookEnv) {
	case "":
		return
	case hookModeMove:
		if err := runAttachHook(os.Stdin); err != nil {
			fmt.Fprintf(os.Stderr, "spacenet move hook: %v\n", err)
			os.Exit(1)
		}
	case hookModeConfigure:
		if err := configurePeer(commandFromHookEnv()); err != nil {
			fmt.Fprintf(os.Stderr, "spacenet configure hook: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "spacenet hook: unknown mode %q\n", os.Getenv(hookEnv))
		os.Exit(1)
	}
	os.Exit(0)
}

// runAttachHook reads the OCI state from r, builds the attach request from the
// hook env, sends it to the worker's attach socket, and returns the worker's
// result. Split from MaybeRunAttachHook so it is unit-testable (feed a state
// JSON + a fake socket, assert the request the worker receives).
func runAttachHook(r io.Reader) error {
	var st ociState
	if err := json.NewDecoder(r).Decode(&st); err != nil {
		return fmt.Errorf("decode OCI state: %w", err)
	}
	if st.Pid <= 0 {
		return fmt.Errorf("OCI state has no init pid")
	}
	sock := os.Getenv(hookEnvSock)
	if sock == "" {
		return fmt.Errorf("no attach socket in hook env")
	}
	req := hookAttachRequest{
		Pid:    st.Pid,
		Name:   os.Getenv(hookEnvName),
		IfName: os.Getenv(hookEnvIf),
		CIDR:   os.Getenv(hookEnvCIDR),
		GwIP:   os.Getenv(hookEnvGw),
		Host:   os.Getenv(hookEnvHost),
		Peer:   os.Getenv(hookEnvPeer),
	}
	return sendHookAttach(sock, req)
}

func commandFromHookEnv() command {
	var allowHost []string
	_ = json.Unmarshal([]byte(os.Getenv(hookEnvAllowHost)), &allowHost)
	return command{
		IF:        os.Getenv(hookEnvIf),
		CIDR:      os.Getenv(hookEnvCIDR),
		GwIP:      os.Getenv(hookEnvGw),
		Host:      os.Getenv(hookEnvHost),
		Peer:      os.Getenv(hookEnvPeer),
		AllowHost: allowHost,
	}
}

// sendHookAttach dials the worker attach socket, writes one request, and reads
// the single JSON reply. A non-OK reply is returned as an error so the hook
// exits non-zero (failing runc create). The deadline bounds a wedged worker so
// the hook never hangs a container create indefinitely.
func sendHookAttach(sock string, req hookAttachRequest) error {
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial attach socket %q: %w", sock, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write attach request: %w", err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read attach reply (worker dead?): %w", err)
	}
	var rep reply
	if err := json.Unmarshal(line, &rep); err != nil {
		return fmt.Errorf("decode attach reply %q: %w", line, err)
	}
	if !rep.OK {
		return fmt.Errorf("attach pid %d: %s", req.Pid, rep.Error)
	}
	return nil
}

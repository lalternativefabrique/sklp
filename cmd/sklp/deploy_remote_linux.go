// deploy_remote.go is the SSH transport for `sklp deploy --host user@server`:
// it runs the deploy ON the remote machine by invoking the remote `sklp` over
// the system `ssh`. There is NO network agent and NO mTLS — distance and
// security live in SSH (the user's ~/.ssh/config, agent and known_hosts apply).
//
// DEPRECATED: prefer the network control API (`--server https://host:9443
// --token ... --ca-fingerprint ...`, see deploy_remote_net.go). The supervisor's
// `--listen :PORT` exposes that API directly; `--host` ssh is kept working for
// back-compat but is no longer the recommended remote path.
// Streaming (transitions, logs, banner) flows back transparently over ssh
// stdout/stderr; `ps`/`logs`/`down` are the same remote invocation.
//
// This file is intentionally NOT gated on embedded_runtime: it is a pure ssh
// client (no runc), so a local workstation built WITHOUT the embedded runtime
// can still pilot a remote server. The remote `sklp` is the one that needs the
// embedded runtime.
//
// Milestone-1 assumptions (documented limits, NOT bugs):
//   - `sklp` is ALREADY present on the remote PATH (bootstrap/scp is a later
//     milestone). Override the remote binary with --remote-bin or SKLP_REMOTE_BIN.
//   - The deploy.yaml is ALREADY on the server: --file/--repo are passed through
//     verbatim as REMOTE paths (local manifest upload via stdin is a later
//     milestone).
//   - Deploy args are simple tokens (space/service names without spaces). ssh
//     reassembles the remote argv into a shell string, so an arg containing a
//     space would re-split remotely; this milestone does not quote-protect that.
package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// sshTarget is the parsed SSH transport: where to connect and how, independent
// of the deploy args that are forwarded to the remote sklp.
type sshTarget struct {
	userHost  string // "user@server" handed to ssh
	port      int    // 0 = ssh default / from ~/.ssh/config
	identity  string // -i key path; "" = ssh default / agent
	remoteBin string // remote sklp binary; "" defaults to "sklp"
}

// isTerminal reports whether stdout AND stdin are TTYs. Swappable in tests so
// the -t/-T decision is unit-testable without a real terminal.
var isTerminal = func() bool {
	return term.IsTerminal(int(os.Stdout.Fd())) && term.IsTerminal(int(os.Stdin.Fd()))
}

// maybeRunRemote inspects deploy args for an SSH transport flag (--host /
// --remote). With none, it returns handled=false and the normal local flow
// continues. With one, it builds and runs the ssh command, relaying stdio and
// the remote exit code; handled=true.
//
// It is called from main.go BEFORE the (possibly stubbed/gated) runDeploy, so
// `--host` works even from a binary built without the embedded runtime.
func maybeRunRemote(args []string) (handled bool, exitCode int) {
	target, deployArgs, ok, err := parseRemoteArgs(args)
	if err != nil {
		printRemoteUsageErr(err.Error())
		return true, 2
	}
	if !ok {
		return false, 0 // no --host/--remote: local flow continues
	}
	cmd := buildSSHCommand(target, deployArgs)
	return true, runSSH(cmd)
}

// runSSH executes the prepared ssh command, wiring local stdio so the remote
// stream is relayed transparently (Ctrl-C propagates via the pty when -t is
// set). Returns the ssh exit code.
func runSSH(cmd *exec.Cmd) int {
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		// ssh itself failed to start (not found, etc.): surface it.
		printRemoteUsageErr("ssh: " + err.Error())
		return 1
	}
	return 0
}

// parseRemoteArgs permissively scans the deploy args for the transport flags
// (--host/-host user@server, --remote/-remote ssh://user@server[:port],
// --ssh-port N, --identity path, --remote-bin name) and strips them, returning
// the remote sklp deploy args (everything deploy-related kept intact). ok is
// false when no --host/--remote is present.
func parseRemoteArgs(args []string) (target sshTarget, deployArgs []string, ok bool, err error) {
	deployArgs = make([]string, 0, len(args))
	target.remoteBin = remoteBinDefault()

	// value reads the inline (=v) or next-token value for a transport flag.
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, inline, hasInline := splitFlag(a)
		take := func() (string, bool) {
			if hasInline {
				return inline, true
			}
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		switch name {
		case "host":
			v, gotv := take()
			if !gotv || v == "" {
				return target, nil, false, errMissingValue("--host")
			}
			target.userHost = v
			ok = true
		case "remote":
			v, gotv := take()
			if !gotv || v == "" {
				return target, nil, false, errMissingValue("--remote")
			}
			uh, port, perr := parseRemoteURL(v)
			if perr != nil {
				return target, nil, false, perr
			}
			target.userHost = uh
			if port != 0 {
				target.port = port
			}
			ok = true
		case "ssh-port":
			v, gotv := take()
			if !gotv {
				return target, nil, false, errMissingValue("--ssh-port")
			}
			p, cerr := strconv.Atoi(v)
			if cerr != nil || p <= 0 {
				return target, nil, false, &remoteArgError{"invalid --ssh-port " + strconv.Quote(v)}
			}
			target.port = p
		case "identity":
			v, gotv := take()
			if !gotv || v == "" {
				return target, nil, false, errMissingValue("--identity")
			}
			target.identity = v
		case "remote-bin":
			v, gotv := take()
			if !gotv || v == "" {
				return target, nil, false, errMissingValue("--remote-bin")
			}
			target.remoteBin = v
		default:
			// Not a transport flag: forward it verbatim to the remote sklp.
			deployArgs = append(deployArgs, a)
		}
	}
	return target, deployArgs, ok, nil
}

// splitFlag normalises a "-flag" / "--flag" / "--flag=value" token into its
// bare name (no dashes) and inline value. A non-flag token yields name "".
func splitFlag(a string) (name, value string, hasInline bool) {
	if len(a) < 2 || a[0] != '-' {
		return "", "", false
	}
	a = strings.TrimLeft(a, "-")
	if k, v, found := strings.Cut(a, "="); found {
		return k, v, true
	}
	return a, "", false
}

// parseRemoteURL parses --remote ssh://user@server[:port] into the user@host
// and optional port. A bare "user@server[:port]" (no scheme) is also accepted.
func parseRemoteURL(raw string) (userHost string, port int, err error) {
	s := strings.TrimPrefix(raw, "ssh://")
	host := s
	if at := strings.LastIndex(s, "@"); at >= 0 {
		host = s[at+1:]
	}
	// A ":" in the host part is a port (IPv6 literals are out of scope here).
	if colon := strings.LastIndex(host, ":"); colon >= 0 {
		p, cerr := strconv.Atoi(host[colon+1:])
		if cerr != nil || p <= 0 {
			return "", 0, &remoteArgError{"invalid port in --remote " + strconv.Quote(raw)}
		}
		port = p
		s = s[:len(s)-(len(host)-colon)]
	}
	if s == "" {
		return "", 0, &remoteArgError{"empty --remote target"}
	}
	return s, port, nil
}

// buildSSHCommand assembles the system-ssh invocation that runs the deploy on
// the remote host:
//
//	ssh [-t|-T] [-p port] [-i identity] user@server -- <remoteBin> deploy <deployArgs>
//
// A pseudo-tty (-t) is requested only for the LIVE sub-verbs (deploy / delete /
// logs -f) AND only when the local side is a TTY, so piped/CI output stays
// clean (-T). Read-only `ps` and non-follow `logs` never get a pty.
func buildSSHCommand(target sshTarget, deployArgs []string) *exec.Cmd {
	sshArgs := make([]string, 0, 8+len(deployArgs))

	if wantsTTY(deployArgs) && isTerminal() {
		sshArgs = append(sshArgs, "-t")
	} else {
		sshArgs = append(sshArgs, "-T") // explicit: never allocate a pty
	}
	if target.port != 0 {
		sshArgs = append(sshArgs, "-p", strconv.Itoa(target.port))
	}
	if target.identity != "" {
		sshArgs = append(sshArgs, "-i", target.identity)
	}
	sshArgs = append(sshArgs, target.userHost, "--", target.remoteBin, "deploy")
	sshArgs = append(sshArgs, deployArgs...)

	return exec.Command("ssh", sshArgs...)
}

// wantsTTY reports whether the remote deploy invocation streams live output
// that benefits from a pseudo-tty: the deploy itself (no read-only sub-verb) or
// `logs -f`. `ls`/`get` and non-follow `logs` are capturable and stay pty-free.
func wantsTTY(deployArgs []string) bool {
	verb := leadingSubverb(deployArgs)
	switch verb {
	case "ls", "get":
		return false
	case "logs":
		return hasFollowFlag(deployArgs)
	case "exec":
		// Interactive shell into a container: always wants a pty so keystrokes,
		// signals and the remote PTY round-trip over ssh -t.
		return true
	case "restart":
		// One-shot ack, no live stream: no pty needed.
		return false
	default:
		// deploy (no sub-verb) and `delete`: live banner / teardown stream.
		return true
	}
}

// leadingSubverb returns the first non-flag token of the deploy args (the
// sub-verb: ps/logs/delete), or "" when the first token is a flag (a plain
// deploy, e.g. `--space production`).
func leadingSubverb(deployArgs []string) string {
	for _, a := range deployArgs {
		if a == "" {
			continue
		}
		if a[0] == '-' {
			return "" // a flag first => plain deploy
		}
		return a
	}
	return ""
}

// wantsHelp reports whether the args carry a help request (-h / --help / help),
// so the remote shims can defer to the façade's sub-verb catalogue.
func wantsHelp(args []string) bool {
	for _, a := range args {
		switch a {
		case "-h", "--help", "help":
			return true
		}
	}
	return false
}

// hasFollowFlag reports whether the args carry the logs follow flag (-f /
// --follow), matching the remote `deploy logs` flag set.
func hasFollowFlag(deployArgs []string) bool {
	for _, a := range deployArgs {
		switch a {
		case "-f", "--f", "-follow", "--follow":
			return true
		}
	}
	return false
}

// remoteBinDefault picks the remote sklp binary name: SKLP_REMOTE_BIN when set,
// else "sklp" (assumed on the remote PATH). The --remote-bin flag overrides it.
func remoteBinDefault() string {
	if v := os.Getenv("SKLP_REMOTE_BIN"); v != "" {
		return v
	}
	return "sklp"
}

// remoteArgError is a transport-arg parse failure surfaced as a usage error.
type remoteArgError struct{ msg string }

func (e *remoteArgError) Error() string { return e.msg }

func errMissingValue(flag string) error {
	return &remoteArgError{"missing value for " + flag}
}

func printRemoteUsageErr(msg string) {
	os.Stderr.WriteString("sklp deploy --host: " + msg + "\n")
}

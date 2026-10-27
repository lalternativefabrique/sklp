// auth.go implements `sklp auth {login,status,logout}` — interactive
// management of the CLI's Personal Access Token. The on-disk config is
// owned by the internal/config package; this file is purely the verb
// surface and the user-facing flow.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/lalternativefabrique/sklp/cmd/sklp/internal/apiclient"
	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/config"
	"golang.org/x/term"
)

// authIO bundles the streams every auth subcommand reads from and
// writes to. Injectable so tests can drive the flow without a TTY.
type authIO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// ReadPassword reads a line from stdin without echoing. When nil
	// (production), falls back to golang.org/x/term on the FD of Stdin
	// if it's a TTY, otherwise to a plain bufio read.
	ReadPassword func() (string, error)
}

func defaultAuthIO() *authIO {
	return &authIO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
}

// authCommand declares `sklp auth <login|status|logout>` on the façade. All
// three operate on the single "api" config block — there is no profile to
// select, so no --profile flag; login accepts --api-url to pre-fill the
// prompt. Run is thin transport glue mapping a returned error to exit code 1.
func authCommand() cli.Command {
	authRun := func(fn func(*authIO) error) func(context.Context, cli.Args) int {
		return func(ctx context.Context, args cli.Args) int {
			if err := fn(defaultAuthIO()); err != nil {
				fmt.Fprintln(os.Stderr, "sklp auth:", err)
				return 1
			}
			return 0
		}
	}
	return cli.Command{
		Name:  "auth",
		Short: "Manage CLI authentication against the Skalpai API: login | status | logout",
		Sub: []cli.Command{
			{
				Name:  "login",
				Short: "Authenticate against the Skalpai API and save the token",
				Flags: []cli.Flag{
					{Name: "api-url", Type: cli.StringFlag, Usage: "pre-fill the API URL prompt with this value"},
				},
				Run: func(ctx context.Context, args cli.Args) int {
					if err := authLogin(defaultAuthIO(), args.String("api-url")); err != nil {
						fmt.Fprintln(os.Stderr, "sklp auth:", err)
						return 1
					}
					return 0
				},
			},
			{
				Name:  "status",
				Short: "Show the configured API endpoint and validate the saved token",
				Run:   authRun(authStatus),
			},
			{
				Name:  "logout",
				Short: "Remove the saved API token (keeps the url, leaves deploy contexts untouched)",
				Run:   authRun(authLogout),
			},
		},
	}
}

// authLogin walks the user through saving the SaaS PAT:
// prompt URL → prompt token → validate against GET /api/me → save.
//
// We read cfg.API directly rather than through ResolveAPI: a not-yet-
// configured api block is the normal first-login case, not an error.
func authLogin(io *authIO, apiURLFlag string) error {
	cfg, path, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// 1) API URL prompt.
	defaultURL := cfg.API.URL
	if apiURLFlag != "" {
		defaultURL = apiURLFlag
	}
	if defaultURL == "" {
		defaultURL = config.DefaultAPIURL
	}
	fmt.Fprintf(io.Stdout, "API URL [%s]: ", defaultURL)
	apiURL, err := readLine(io.Stdin)
	if err != nil {
		return fmt.Errorf("read api url: %w", err)
	}
	apiURL = strings.TrimSpace(apiURL)
	if apiURL == "" {
		apiURL = defaultURL
	}

	// 2) Token prompt — masked.
	fmt.Fprint(io.Stdout, "Token (will be masked): ")
	token, err := readPassword(io)
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	fmt.Fprintln(io.Stdout) // newline after silent input
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("aborted: empty token")
	}
	if !strings.HasPrefix(token, "sklp_pat_") {
		return errors.New("tokens must start with sklp_pat_ — generate one in the web app under Settings → Personal Access Tokens")
	}

	// 3) Validate via GET /api/me (JWTOrPAT). The endpoint returns
	// {user_id, email, role}; we surface the email back to the user.
	cli := apiclient.New(apiURL, token)
	body, status, err := cli.Get(context.Background(), "/api/me")
	if err != nil {
		if status == http.StatusUnauthorized {
			return errors.New("token rejected by API")
		}
		return fmt.Errorf("validate token: %w", err)
	}
	var me meResponse
	if jerr := json.Unmarshal(body, &me); jerr != nil {
		// Non-fatal: we still know auth succeeded. Fall through with empty email.
		me = meResponse{}
	}

	// 4) Save.
	cfg.SetAPI(config.API{URL: apiURL, Token: token})
	if err := cfg.Save(path); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	who := me.Email
	if who == "" {
		who = "authenticated user"
	}
	fmt.Fprintf(io.Stdout, "✓ logged in as %s · %s\n", who, apiURL)
	return nil
}

// authStatus pings /api/me with the saved token and prints a one-line
// summary. Exits non-zero (via returned error) if not logged in or if
// the token is rejected.
func authStatus(io *authIO) error {
	cfg, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	api, err := cfg.ResolveAPI()
	if err != nil {
		return err
	}
	token := config.EffectiveAPIToken(api)
	if token == "" {
		return errors.New("not logged in (run sklp auth login)")
	}

	cli := apiclient.New(api.URL, token)
	body, status, err := cli.Get(context.Background(), "/api/me")
	if err != nil {
		if status == http.StatusUnauthorized {
			return errors.New("token rejected by the API (run sklp auth login to save a fresh one)")
		}
		return fmt.Errorf("status check: %w", err)
	}
	var me meResponse
	_ = json.Unmarshal(body, &me)

	fmt.Fprintf(io.Stdout, "%s · %s · token %s\n",
		me.Email, api.URL, maskTokenForDisplay(token))
	return nil
}

// authLogout strips the SaaS token and keeps the URL. Deploy context tokens
// are untouched — logging out of the SaaS must not disarm the deploy
// transport (see config.ClearAPIToken).
func authLogout(io *authIO) error {
	cfg, path, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.API.Token == "" {
		return errors.New("not logged in — nothing to do")
	}
	cfg.ClearAPIToken()
	if err := cfg.Save(path); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintln(io.Stdout, "✓ logged out — api url kept, deploy contexts untouched")
	return nil
}

// meResponse mirrors the GET /api/me payload. Kept minimal — the CLI
// only needs the email to greet the user.
type meResponse struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
}

// readLine reads a single newline-terminated line from r without
// stripping internal whitespace; caller TrimSpace's as needed.
func readLine(r io.Reader) (string, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return line, nil
}

// readPassword reads a token without echoing if stdin is a TTY (real
// user), falling back to a plain line read otherwise (pipes, tests).
// Tests can override via authIO.ReadPassword.
func readPassword(aio *authIO) (string, error) {
	if aio.ReadPassword != nil {
		return aio.ReadPassword()
	}
	if f, ok := aio.Stdin.(*os.File); ok {
		fd := int(f.Fd())
		if term.IsTerminal(fd) {
			b, err := term.ReadPassword(fd)
			if err != nil {
				return "", err
			}
			return string(b), nil
		}
	}
	return readLine(aio.Stdin)
}

// maskTokenForDisplay shows the recognisable prefix and the last 4
// chars so users can recognise which token is in play without exposing
// the full secret. Mirrors the masking used in the Settings UI.
func maskTokenForDisplay(token string) string {
	if len(token) <= 8 {
		return "****"
	}
	// Keep the "sklp_pat_" prefix when present plus next 4, then last 4.
	if strings.HasPrefix(token, "sklp_pat_") && len(token) >= 17 {
		return token[:13] + "…" + token[len(token)-4:]
	}
	return token[:4] + "…" + token[len(token)-4:]
}

package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/restore/drill"
)

// kinds keeps the names the Health screen already groups checks by, so a
// drill replaces the scripts it supersedes without splitting their history.
var kinds = map[string]string{
	"postgres": "pgbackrest-restore",
	"nats":     "nats-restore",
}

type payload struct {
	Space       string       `json:"space"`
	Stanza      string       `json:"stanza"`
	Kind        string       `json:"kind"`
	Status      drill.Status `json:"status"`
	StartedAt   time.Time    `json:"started_at"`
	FinishedAt  time.Time    `json:"finished_at"`
	BackupLabel string       `json:"backup_label"`
	RestoredLSN string       `json:"restored_lsn,omitempty"`
	Tables      int          `json:"tables"`
	Detail      string       `json:"detail"`
	Report      drill.Report `json:"report"`
}

func Kind(engine string) string {
	if k, ok := kinds[engine]; ok {
		return k
	}
	return engine + "-restore"
}

type Client struct {
	Endpoint string
	APIKey   string
	HTTP     *http.Client
	Attempts int
	Backoff  time.Duration
}

func (c Client) Send(ctx context.Context, r drill.Report) error {
	body, err := json.Marshal(payload{
		Space:       r.Space,
		Stanza:      r.Target,
		Kind:        Kind(r.Engine),
		Status:      r.Status,
		StartedAt:   r.StartedAt,
		FinishedAt:  r.FinishedAt,
		BackupLabel: r.Backup.Label,
		RestoredLSN: r.Inventory.Position,
		Tables:      len(r.Inventory.Collections),
		Detail:      r.Summary(),
		Report:      r,
	})
	if err != nil {
		return err
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	attempts, backoff := max(1, c.Attempts), c.Backoff
	var last error
	for i := range attempts {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff << (i - 1)):
			}
		}
		last = c.post(ctx, httpc, body)
		if last == nil || !retryable(last) {
			return last
		}
	}
	return last
}

type statusError struct {
	code int
	body string
}

func (e statusError) Error() string { return fmt.Sprintf("core answered %d: %s", e.code, e.body) }

func retryable(err error) bool {
	se, ok := err.(statusError)
	return !ok || se.code >= 500 || se.code == http.StatusTooManyRequests
}

func (c Client) post(ctx context.Context, httpc *http.Client, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.Endpoint, "/")+"/api/v1/backup-checks", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return statusError{code: resp.StatusCode, body: strings.TrimSpace(string(msg))}
}

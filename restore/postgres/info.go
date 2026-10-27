package postgres

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/lalternativefabrique/sklp/restore/drill"
)

type stanzaInfo struct {
	Name   string `json:"name"`
	Status struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"status"`
	DB []struct {
		ID      int    `json:"id"`
		Version string `json:"version"`
	} `json:"db"`
	Backup []struct {
		Label     string `json:"label"`
		Type      string `json:"type"`
		Error     bool   `json:"error"`
		Timestamp struct {
			Stop int64 `json:"stop"`
		} `json:"timestamp"`
		Info struct {
			Size int64 `json:"size"`
		} `json:"info"`
		Database struct {
			ID int `json:"id"`
		} `json:"database"`
	} `json:"backup"`
}

type located struct {
	drill.Backup
	Major int
}

func newestBackup(raw []byte, stanza string) (located, error) {
	var stanzas []stanzaInfo
	if err := json.Unmarshal(raw, &stanzas); err != nil {
		return located{}, fmt.Errorf("decode pgbackrest info: %w", err)
	}
	for _, s := range stanzas {
		if s.Name != stanza {
			continue
		}
		if s.Status.Code != 0 && len(s.Backup) == 0 {
			return located{}, fmt.Errorf("stanza %s: %s", stanza, s.Status.Message)
		}
		for i := len(s.Backup) - 1; i >= 0; i-- {
			b := s.Backup[i]
			if b.Error {
				continue
			}
			major := 0
			for _, db := range s.DB {
				if db.ID == b.Database.ID {
					major = majorOf(db.Version)
				}
			}
			return located{
				Backup: drill.Backup{Label: b.Label, Type: b.Type, TakenAt: time.Unix(b.Timestamp.Stop, 0).UTC(), Bytes: b.Info.Size},
				Major:  major,
			}, nil
		}
		return located{}, fmt.Errorf("%w in stanza %s", drill.ErrNoBackup, stanza)
	}
	return located{}, fmt.Errorf("%w: stanza %s is not in the repository", drill.ErrNoBackup, stanza)
}

var versionRE = regexp.MustCompile(`(\d+)(?:\.\d+)?`)

func majorOf(v string) int {
	m := versionRE.FindStringSubmatch(v)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

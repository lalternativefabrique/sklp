package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

func TestDataStoresRouteGradesTheLiveSpaceWithoutLeakingSecrets(t *testing.T) {
	sup := &fakeSupervisor{live: map[string]spacemanager.Service{
		"postgres": {Name: "postgres", Image: "postgres:17-alpine", Volumes: []string{"pgdata:/var/lib/postgresql/data"},
			Env: []string{"PGBACKREST_STANZA=shop", "PGBACKREST_REPO1_S3_BUCKET=shop-backups", "PGBACKREST_REPO1_S3_KEY_SECRET=s3cr3t"}},
		"cache": {Name: "cache", Image: "redis:7", Volumes: []string{"redis:/data"}},
		"web":   {Name: "web", Image: "shop/web:1"},
	}}
	rec := httptest.NewRecorder()
	newMux("shop-production", sup, func() {}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, RouteDataStores, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "s3cr3t") {
		t.Fatal("the inventory leaked a backup credential")
	}
	var got DataStoresResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	protection := map[string]spacemanager.Protection{}
	for _, s := range got.Stores {
		protection[s.Service] = s.Protection
	}
	if got.Space != "shop-production" || len(got.Stores) != 2 ||
		protection["postgres"] != spacemanager.ProtectionBacked || protection["cache"] != spacemanager.ProtectionNone {
		t.Fatalf("response = %+v", got)
	}
}

func TestDataStoresRouteIsReadOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	newMux("s", &fakeSupervisor{}, func() {}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RouteDataStores, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST answered %d", rec.Code)
	}
}

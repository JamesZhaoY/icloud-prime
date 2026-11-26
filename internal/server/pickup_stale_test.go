package server

import (
	"testing"
	"time"

	"icloud-hme/internal/hme"
)

func ageAliasCache(srv *Server, accountID string, age time.Duration) {
	srv.cache.mu.Lock()
	defer srv.cache.mu.Unlock()
	entry := srv.cache.aliases[accountID]
	entry.fetched = time.Now().Add(-age)
	entry.expires = entry.fetched.Add(srv.cacheTTL)
	srv.cache.aliases[accountID] = entry
}

func TestStaleAliasesServeWithinPickupWindow(t *testing.T) {
	srv := newCreateJobTestServer(t)
	srv.setCachedAliases("acc", []hme.Alias{{Email: "a@example.com", Active: true}})

	ageAliasCache(srv, "acc", srv.cacheTTL+time.Minute)
	if _, fresh := srv.cachedAliases("acc"); fresh {
		t.Fatal("entry older than cacheTTL must not count as fresh")
	}
	got, ok := srv.staleAliases("acc", srv.pickupStaleTTL)
	if !ok || len(got) != 1 || got[0].Email != "a@example.com" {
		t.Fatalf("stale aliases within the pickup window: ok=%t %v", ok, got)
	}

	ageAliasCache(srv, "acc", srv.pickupStaleTTL+time.Minute)
	if _, ok := srv.staleAliases("acc", srv.pickupStaleTTL); ok {
		t.Fatal("aliases older than pickupStaleTTL must not be used")
	}
}

func TestStaleAliasesGoneAfterInvalidate(t *testing.T) {
	srv := newCreateJobTestServer(t)
	srv.setCachedAliases("acc", []hme.Alias{{Email: "a@example.com", Active: true}})
	srv.invalidateAliases("acc")
	if _, ok := srv.staleAliases("acc", srv.pickupStaleTTL); ok {
		t.Fatal("deactivate/delete invalidation must also drop the stale copy")
	}
}

func TestBackgroundAliasRefreshRunsOncePerAccount(t *testing.T) {
	srv := newCreateJobTestServer(t)
	srv.aliasRefresh.Store("acc", struct{}{})
	srv.refreshAliasesInBackground("acc")
	if _, running := srv.aliasRefresh.Load("acc"); !running {
		t.Fatal("a refresh already in flight must not be cleared by a second call")
	}
}

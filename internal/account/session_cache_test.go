package account

import (
	"testing"

	"icloud-hme/internal/hme"
)

func newCookieAccount(t *testing.T) (*Manager, string, map[string]string) {
	t.Helper()
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	acc, err := mgr.AddAccount("test", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cookies := map[string]string{"session": "v"}
	if err := mgr.SaveCookies(acc.ID, cookies); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.resetNetworkCaches)
	return mgr, acc.ID, cookies
}

func TestHMEClientReusesValidatedSession(t *testing.T) {
	mgr, id, _ := newCookieAccount(t)
	first, err := mgr.HMEClient(id, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Session().ServiceURL != "" {
		t.Fatal("first client must validate")
	}
	first.OnValidated(hme.Session{ServiceURL: "https://p1-maildomainws.icloud.com", DSID: "1"})
	second, err := mgr.HMEClient(id, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Session(); got.ServiceURL != "https://p1-maildomainws.icloud.com" || got.DSID != "1" {
		t.Fatalf("second client did not reuse session: %+v", got)
	}
	mgr.cacheMu.Lock()
	transports := len(mgr.transports)
	mgr.cacheMu.Unlock()
	if transports != 1 {
		t.Fatalf("expected one shared transport per account, got %d", transports)
	}
}

func TestRejectedCookiesDropCachedSession(t *testing.T) {
	mgr, id, cookies := newCookieAccount(t)
	mgr.storeSession(id, "icloud.com", hme.Session{ServiceURL: "https://p1-maildomainws.icloud.com"})
	if err := mgr.SaveCookiesIfUnchanged(id, cookies, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.cachedSession(id, "icloud.com"); ok {
		t.Fatal("session must be dropped when Apple clears the cookies")
	}
}

func TestReloadAndRemoveDropNetworkCaches(t *testing.T) {
	mgr, id, _ := newCookieAccount(t)
	if _, err := mgr.HMEClient(id, false); err != nil {
		t.Fatal(err)
	}
	mgr.storeSession(id, "icloud.com", hme.Session{ServiceURL: "https://p1-maildomainws.icloud.com"})
	if err := mgr.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.cachedSession(id, "icloud.com"); ok {
		t.Fatal("Reload must drop cached sessions")
	}
	if _, err := mgr.HMEClient(id, false); err != nil {
		t.Fatal(err)
	}
	mgr.storeSession(id, "icloud.com", hme.Session{ServiceURL: "https://p1-maildomainws.icloud.com"})
	if removed, err := mgr.RemoveAccount(id); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	mgr.cacheMu.Lock()
	_, hasSession := mgr.sessions[id]
	_, hasTransport := mgr.transports[id]
	mgr.cacheMu.Unlock()
	if hasSession || hasTransport {
		t.Fatal("RemoveAccount must drop the account's session and transport")
	}
}

func TestSessionCacheIsPerHost(t *testing.T) {
	mgr, id, _ := newCookieAccount(t)
	mgr.storeSession(id, "icloud.com.cn", hme.Session{ServiceURL: "https://p1-maildomainws.icloud.com.cn"})
	client, err := mgr.HMEClient(id, false)
	if err != nil {
		t.Fatal(err)
	}
	if client.Session().ServiceURL != "" {
		t.Fatal("a session from another host must not be reused")
	}
}

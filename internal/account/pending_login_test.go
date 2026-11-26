package account

import (
	"errors"
	"testing"
	"time"

	"icloud-hme/internal/hme"
)

func TestCompleteLoginOTPWithoutPendingLogin(t *testing.T) {
	mgr, id, _ := newCookieAccount(t)
	if err := mgr.CompleteLoginOTP(id, "123456"); !errors.Is(err, ErrNoPendingLogin) {
		t.Fatalf("want ErrNoPendingLogin, got %v", err)
	}
}

func TestPendingLoginIsOneTimeAndExpires(t *testing.T) {
	mgr, id, _ := newCookieAccount(t)
	client := &hme.Client{}
	mgr.stashPendingLogin(id, client)
	if got, ok := mgr.takePendingLogin(id); !ok || got != client {
		t.Fatal("stashed login not returned")
	}
	if _, ok := mgr.takePendingLogin(id); ok {
		t.Fatal("pending login must be single use")
	}

	mgr.stashPendingLogin(id, client)
	mgr.cacheMu.Lock()
	entry := mgr.pendingLogins[id]
	entry.expires = time.Now().Add(-time.Second)
	mgr.pendingLogins[id] = entry
	mgr.cacheMu.Unlock()
	if _, ok := mgr.takePendingLogin(id); ok {
		t.Fatal("expired pending login must not be returned")
	}
}

func TestResetNetworkCachesDropsPendingLogins(t *testing.T) {
	mgr, id, _ := newCookieAccount(t)
	mgr.stashPendingLogin(id, &hme.Client{})
	mgr.resetNetworkCaches()
	if _, ok := mgr.takePendingLogin(id); ok {
		t.Fatal("reset must drop pending logins")
	}
}

func TestRequestLoginSMSWithoutPendingLogin(t *testing.T) {
	mgr, id, _ := newCookieAccount(t)
	if _, err := mgr.RequestLoginSMS(id); !errors.Is(err, ErrNoPendingLogin) {
		t.Fatalf("want ErrNoPendingLogin, got %v", err)
	}
}

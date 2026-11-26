package server

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactLoginErrorKeepsStepAndStatus(t *testing.T) {
	got := redactLoginError(errors.New("auth init: auth init failed: HTTP 503"))
	if got != "auth init: auth init failed: HTTP 503" {
		t.Fatalf("got %q", got)
	}
}

func TestRedactLoginErrorStripsQueryAndProxyCredentials(t *testing.T) {
	err := errors.New(`auth start: Get "https://idmsa.apple.com/appleauth/auth/authorize/signin?frame_id=auth-x&client_id=abc": proxyconnect tcp: dial socks5://user:hunter2@10.0.0.1:1080 refused`)
	got := redactLoginError(err)
	for _, secret := range []string{"hunter2", "user:", "client_id", "frame_id"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted message still contains %q: %q", secret, got)
		}
	}
	if !strings.Contains(got, "auth start") || !strings.Contains(got, "idmsa.apple.com") {
		t.Fatalf("redaction removed the useful part: %q", got)
	}
}

func TestRedactLoginErrorLimitsLength(t *testing.T) {
	got := redactLoginError(errors.New(strings.Repeat("错", 500)))
	if n := len([]rune(got)); n > 161 {
		t.Fatalf("message too long: %d runes", n)
	}
}

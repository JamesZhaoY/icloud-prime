package hme

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
)

// loginWith2FA runs a password login whose signin/complete asks for 2FA.
// verifyHeaders go on the securitycode response, trustHeaders on the 2sv/trust response.
func loginWith2FA(t *testing.T, verifyHeaders, trustHeaders map[string]string, accountLogin func(*http.Request) *http.Response) (*Client, error, bool) {
	t.Helper()
	real, err := NewClient(nil, "icloud.com.cn", "", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(real.httpc.CloseIdleConnections)
	called := false
	real.httpc = mockHTTP{HttpClient: real.httpc, do: func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/signin/init"):
			return mockResponse(200, `{"iteration":1,"salt":"AQ==","b":"Ag==","c":"c","protocol":"s2k"}`), nil
		case strings.HasSuffix(req.URL.Path, "/signin/complete"):
			resp := mockResponse(409, `{}`)
			resp.Header.Set("scnt", "scnt-1")
			resp.Header.Set("X-Apple-ID-Session-Id", "sid-1")
			return resp, nil
		case strings.Contains(req.URL.Path, "/securitycode"):
			resp := mockResponse(204, ``)
			for k, v := range verifyHeaders {
				resp.Header.Set(k, v)
			}
			return resp, nil
		case strings.HasSuffix(req.URL.Path, "/2sv/trust"):
			resp := mockResponse(204, ``)
			for k, v := range trustHeaders {
				resp.Header.Set(k, v)
			}
			return resp, nil
		case strings.HasSuffix(req.URL.Path, "/accountLogin"):
			called = true
			resp := accountLogin(req)
			if resp.StatusCode == 200 {
				resp.Header.Add("Set-Cookie", "X-APPLE-WEBAUTH-TOKEN=web; Path=/")
			}
			return resp, nil
		}
		return mockResponse(200, `{}`), nil
	}}
	err = real.Login("user@example.com", "pw", func() (string, error) { return "123456", nil })
	return real, err, called
}

func TestSessionTokenFromVerifySurvivesTrustWithoutToken(t *testing.T) {
	var sent map[string]any
	c, err, called := loginWith2FA(t,
		map[string]string{"X-Apple-Session-Token": "from-verify", "X-Apple-ID-Account-Country": "CHN"},
		map[string]string{"X-Apple-TwoSV-Trust-Token": "trust-tok"},
		func(req *http.Request) *http.Response {
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &sent)
			return mockResponse(200, `{"dsInfo":{"dsid":"7"}}`)
		})
	if err != nil || !called {
		t.Fatalf("login err=%v accountLogin called=%t", err, called)
	}
	if sent["dsWebAuthToken"] != "from-verify" {
		t.Fatalf("trust response without a token wiped the session token: sent %v", sent["dsWebAuthToken"])
	}
	if sent["trustToken"] != "trust-tok" || sent["accountCountryCode"] != "CHN" {
		t.Fatalf("unexpected accountLogin body %v", sent)
	}
	if c.SessionToken() != "from-verify" || c.TrustToken() != "trust-tok" {
		t.Fatalf("client kept session=%q trust=%q", c.SessionToken(), c.TrustToken())
	}
}

func TestTrustTokenUpdatesWhenPresent(t *testing.T) {
	var sent map[string]any
	_, err, _ := loginWith2FA(t,
		map[string]string{"X-Apple-Session-Token": "old"},
		map[string]string{"X-Apple-Session-Token": "new", "X-Apple-TwoSV-Trust-Token": "trust"},
		func(req *http.Request) *http.Response {
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &sent)
			return mockResponse(200, `{}`)
		})
	if err != nil {
		t.Fatal(err)
	}
	if sent["dsWebAuthToken"] != "new" {
		t.Fatalf("later non-empty token must win, sent %v", sent["dsWebAuthToken"])
	}
}

func TestMissingSessionTokenFailsBeforeAccountLogin(t *testing.T) {
	_, err, called := loginWith2FA(t, nil, nil, func(*http.Request) *http.Response {
		return mockResponse(200, `{}`)
	})
	if err == nil || !strings.Contains(err.Error(), "session token") {
		t.Fatalf("expected a missing-session-token error, got %v", err)
	}
	if called {
		t.Fatal("accountLogin must not be called without a session token")
	}
}

func TestAccountLogin400ReportsAppleFieldsWithoutSecrets(t *testing.T) {
	_, err, _ := loginWith2FA(t,
		map[string]string{"X-Apple-Session-Token": "s"},
		map[string]string{"X-Apple-TwoSV-Trust-Token": "t"},
		func(*http.Request) *http.Response {
			return mockResponse(400, `{"success":false,"error":"Missing required field","errorCode":"MISSING_FIELD","trustTokens":["SECRET-VALUE"],"dsWebAuthToken":"SECRET-2"}`)
		})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{"HTTP 400", "errorCode=MISSING_FIELD", "error=Missing required field", "country=CHN", "trust token=true"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q is missing %q", msg, want)
		}
	}
	for _, secret := range []string{"SECRET-VALUE", "SECRET-2", "trustTokens"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("error leaked %q: %q", secret, msg)
		}
	}
}

func TestAppleErrorSummaryHandlesServiceErrorsAndJunk(t *testing.T) {
	got := appleErrorSummary([]byte(`{"serviceErrors":[{"code":"-20209","message":"Your account is locked","suppressDismissal":false}]}`))
	if !strings.Contains(got, "serviceError.code=-20209") || !strings.Contains(got, "Your account is locked") {
		t.Fatalf("got %q", got)
	}
	if appleErrorSummary([]byte(`<html>bad gateway</html>`)) != "" {
		t.Fatal("non-JSON bodies must produce no summary")
	}
}

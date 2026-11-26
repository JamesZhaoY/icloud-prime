package hme

import (
	"errors"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
)

// twoPhaseMock answers a 2FA login; codeOK decides whether the submitted code is accepted.
func twoPhaseMock(t *testing.T, codeOK func(attempt int) bool) (*Client, map[string]int) {
	t.Helper()
	c, err := NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.httpc.CloseIdleConnections)
	hits := map[string]int{}
	c.httpc = mockHTTP{HttpClient: c.httpc, do: func(req *http.Request) (*http.Response, error) {
		p := req.URL.Path
		switch {
		case strings.HasSuffix(p, "/signin/init"):
			hits["init"]++
			return mockResponse(200, `{"iteration":1,"salt":"AQ==","b":"Ag==","c":"c","protocol":"s2k"}`), nil
		case strings.HasSuffix(p, "/signin/complete"):
			hits["complete"]++
			resp := mockResponse(409, `{}`)
			resp.Header.Set("scnt", "scnt-1")
			resp.Header.Set("X-Apple-ID-Session-Id", "sid-1")
			return resp, nil
		case strings.Contains(p, "/securitycode"):
			hits["code"]++
			if req.Header.Get("scnt") != "scnt-1" || req.Header.Get("X-Apple-ID-Session-Id") != "sid-1" {
				t.Errorf("code submitted outside the original handshake: scnt=%q sid=%q",
					req.Header.Get("scnt"), req.Header.Get("X-Apple-ID-Session-Id"))
			}
			for _, h := range []string{"X-Apple-OAuth-Client-Id", "X-Apple-Widget-Key", "X-Apple-OAuth-State", "X-Apple-OAuth-Redirect-URI"} {
				if req.Header.Get(h) == "" {
					t.Errorf("code request is missing %s", h)
				}
			}
			if got := req.Header.Get("Origin"); got != "https://www.icloud.com" {
				t.Errorf("Origin = %q, want the iCloud web origin", got)
			}
			if !codeOK(hits["code"]) {
				return mockResponse(400, `{}`), nil
			}
			resp := mockResponse(204, ``)
			resp.Header.Set("X-Apple-Session-Token", "sess")
			return resp, nil
		case strings.HasSuffix(p, "/2sv/trust"):
			resp := mockResponse(204, ``)
			resp.Header.Set("X-Apple-TwoSV-Trust-Token", "trust")
			return resp, nil
		case strings.HasSuffix(p, "/accountLogin"):
			return accountLoginResp(), nil
		}
		return mockResponse(200, `{}`), nil
	}}
	return c, hits
}

func TestLoginBeginThenCompleteOTPReusesHandshake(t *testing.T) {
	c, hits := twoPhaseMock(t, func(int) bool { return true })
	needs, err := c.LoginBegin("user@example.com", "pw")
	if err != nil || !needs || !c.NeedsOTP() {
		t.Fatalf("LoginBegin needs=%t pending=%t err=%v", needs, c.NeedsOTP(), err)
	}
	if hits["code"] != 0 {
		t.Fatal("LoginBegin must not submit a code")
	}
	if err := c.CompleteOTP("123456"); err != nil {
		t.Fatal(err)
	}
	if hits["init"] != 1 || hits["complete"] != 1 {
		t.Fatalf("code step restarted the handshake: %v", hits)
	}
	if c.NeedsOTP() || c.SessionToken() != "sess" || c.TrustToken() != "trust" {
		t.Fatalf("pending=%t session=%q trust=%q", c.NeedsOTP(), c.SessionToken(), c.TrustToken())
	}
}

func TestWrongCodeKeepsHandshakeForRetry(t *testing.T) {
	c, hits := twoPhaseMock(t, func(attempt int) bool { return attempt > 1 })
	if _, err := c.LoginBegin("user@example.com", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := c.CompleteOTP("000000"); !errors.Is(err, ErrInvalidAppleCode) {
		t.Fatalf("want ErrInvalidAppleCode, got %v", err)
	}
	if !c.NeedsOTP() {
		t.Fatal("wrong code must keep the handshake")
	}
	if err := c.CompleteOTP("123456"); err != nil {
		t.Fatal(err)
	}
	if hits["init"] != 1 {
		t.Fatalf("retry restarted the handshake: %v", hits)
	}
}

func TestCompleteOTPWithoutBeginFails(t *testing.T) {
	c, _ := twoPhaseMock(t, func(int) bool { return true })
	if err := c.CompleteOTP("123456"); err == nil {
		t.Fatal("expected error without a pending handshake")
	}
}

func TestMissingSessionTokenReportsTrustHeaderNamesOnly(t *testing.T) {
	h := http.Header{}
	h.Set("X-Apple-TwoSV-Trust-Token", "SECRET")
	h.Set("Scnt", "SECRET2")
	h.Set("Content-Type", "x")
	got := appleHeaderNames(h)
	if got != "Scnt,Twosv-Trust-Token" {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "SECRET") {
		t.Fatal("header values leaked")
	}
}

package hme

import (
	"testing"

	http "github.com/bogdanfinn/fhttp"
)

const validateOK = `{"webservices":{"premiummailsettings":{"url":"https://p2-maildomainws.icloud.com:443"}},"dsInfo":{"dsid":"7"}}`

// accountLoginResp 模拟 accountLogin 成功后下发新的会话 Cookie。
func accountLoginResp() *http.Response {
	resp := mockResponse(200, `{"dsInfo":{"dsid":"7"}}`)
	resp.Header.Add("Set-Cookie", "X-APPLE-WEBAUTH-TOKEN=fresh; Path=/")
	return resp
}

func TestTokenReauthRecoversFrom421(t *testing.T) {
	// 真实失效场景:缓存会话被拒 → 用现有 Cookie 重新 validate 也被拒(Cookie 已死)
	// → 用长期 token 免密重建 → validate 成功 → 重试列表成功。
	var paths []string
	reauthCalls := 0
	validateCalls := 0
	c := &Client{Cookies: map[string]string{"X-APPLE-WEBAUTH-HSA-TRUST": "t"}, httpc: mockHTTP{do: func(req *http.Request) (*http.Response, error) {
		p := req.URL.Host + req.URL.Path
		paths = append(paths, p)
		switch p {
		case "p1-maildomainws.icloud.com/v2/hme/list":
			return mockResponse(421, `{"success":false,"trustTokens":["leak"]}`), nil
		case "setup.icloud.com/setup/ws/1/validate":
			validateCalls++
			if validateCalls == 1 {
				return mockResponse(421, `{"success":false}`), nil
			}
			return mockResponse(200, validateOK), nil
		case "setup.icloud.com/setup/ws/1/accountLogin":
			return accountLoginResp(), nil
		case "p2-maildomainws.icloud.com/v2/hme/list":
			return mockResponse(200, listOK), nil
		}
		t.Fatalf("unexpected request %s", req.URL)
		return nil, nil
	}}}
	c.UseSession(Session{ServiceURL: "https://p1-maildomainws.icloud.com", DSID: "1"})
	c.Reauth = func() (string, string, bool) {
		reauthCalls++
		return "session-tok", "trust-tok", true
	}
	aliases, err := c.ListAliases()
	if err != nil {
		t.Fatalf("expected recovery via token reauth, got %v", err)
	}
	if len(aliases) != 0 {
		t.Fatalf("unexpected aliases %v", aliases)
	}
	if reauthCalls != 1 {
		t.Fatalf("reauth should be used once, got %d", reauthCalls)
	}
	if c.Cookies["X-APPLE-WEBAUTH-TOKEN"] != "fresh" {
		t.Fatal("refreshed cookie not stored on client")
	}
	want := []string{
		"p1-maildomainws.icloud.com/v2/hme/list",
		"setup.icloud.com/setup/ws/1/validate",
		"setup.icloud.com/setup/ws/1/accountLogin",
		"setup.icloud.com/setup/ws/1/validate",
		"p2-maildomainws.icloud.com/v2/hme/list",
	}
	if len(paths) != len(want) {
		t.Fatalf("requests %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("requests %v, want %v", paths, want)
		}
	}
}

func TestTokenReauthTriedOnceWhenRefreshFails(t *testing.T) {
	logins := 0
	c := &Client{Cookies: map[string]string{"x": "y"}, httpc: mockHTTP{do: func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host + req.URL.Path {
		case "p1-maildomainws.icloud.com/v2/hme/list":
			return mockResponse(421, `{}`), nil
		case "setup.icloud.com/setup/ws/1/validate":
			return mockResponse(421, `{"success":false}`), nil
		case "setup.icloud.com/setup/ws/1/accountLogin":
			logins++
			return mockResponse(421, `{"success":false}`), nil
		}
		t.Fatalf("unexpected request %s after failed refresh", req.URL)
		return nil, nil
	}}}
	c.UseSession(Session{ServiceURL: "https://p1-maildomainws.icloud.com"})
	c.Reauth = func() (string, string, bool) { return "tok", "trust", true }
	if _, err := c.ListAliases(); !IsAuthenticationError(err) {
		t.Fatalf("expected auth error when refresh fails, got %v", err)
	}
	if logins != 1 {
		t.Fatalf("accountLogin must be attempted exactly once, got %d", logins)
	}
}

func TestNoTokenMeansNoReauth(t *testing.T) {
	c := &Client{Cookies: map[string]string{"x": "y"}, httpc: mockHTTP{do: func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v2/hme/list":
			return mockResponse(421, `{}`), nil
		case "/setup/ws/1/validate":
			return mockResponse(421, `{}`), nil
		}
		t.Fatalf("must not call %s without a token", req.URL)
		return nil, nil
	}}}
	c.UseSession(Session{ServiceURL: "https://p1-maildomainws.icloud.com"})
	c.Reauth = func() (string, string, bool) { return "", "", false }
	if _, err := c.ListAliases(); !IsAuthenticationError(err) {
		t.Fatalf("expected auth error, got %v", err)
	}
}

func TestAuthErrorBodyDoesNotLeakCredentials(t *testing.T) {
	c := &Client{serviceURL: "https://p1-maildomainws.icloud.com", httpc: mockHTTP{do: func(req *http.Request) (*http.Response, error) {
		return mockResponse(421, `{"success":false,"trustTokens":["SECRET-TOKEN-VALUE"]}`), nil
	}}}
	_, err := c.ListAliases()
	if err == nil {
		t.Fatal("expected error")
	}
	if contains(err.Error(), "SECRET-TOKEN-VALUE") || contains(err.Error(), "trustTokens") {
		t.Fatalf("auth error leaked response body: %q", err.Error())
	}
}

func TestRefreshWebSessionNeedsWebauthToken(t *testing.T) {
	c := &Client{httpc: mockHTTP{do: func(req *http.Request) (*http.Response, error) {
		// accountLogin succeeds but returns no X-APPLE-WEBAUTH-TOKEN cookie.
		return mockResponse(200, `{"dsInfo":{"dsid":"7"}}`), nil
	}}}
	if err := c.RefreshWebSession("tok", "trust"); err == nil {
		t.Fatal("refresh without a session cookie must fail")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

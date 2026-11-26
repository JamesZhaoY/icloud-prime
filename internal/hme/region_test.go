package hme

import (
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
)

type recordedRequest struct {
	host, path, query string
	body              map[string]any
}

// runMockLogin runs the full password login against a mock Apple, returning every request.
// trustHeaders are added to the /2sv/trust response.
func runMockLogin(t *testing.T, host string, trustHeaders map[string]string) []recordedRequest {
	t.Helper()
	real, err := NewClient(nil, host, "", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(real.httpc.CloseIdleConnections)
	var requests []recordedRequest
	real.httpc = mockHTTP{HttpClient: real.httpc, do: func(req *http.Request) (*http.Response, error) {
		r := recordedRequest{host: req.URL.Host, path: req.URL.Path, query: req.URL.RawQuery}
		if req.Body != nil {
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &r.body)
		}
		requests = append(requests, r)
		switch {
		case strings.HasSuffix(req.URL.Path, "/signin/init"):
			return mockResponse(200, `{"iteration":1,"salt":"AQ==","b":"Ag==","c":"challenge","protocol":"s2k"}`), nil
		case strings.HasSuffix(req.URL.Path, "/2sv/trust"):
			resp := mockResponse(204, ``)
			resp.Header.Set("X-Apple-Session-Token", "session-tok")
			resp.Header.Set("X-Apple-TwoSV-Trust-Token", "trust-tok")
			for k, v := range trustHeaders {
				resp.Header.Set(k, v)
			}
			return resp, nil
		case strings.HasSuffix(req.URL.Path, "/accountLogin"):
			return accountLoginResp(), nil
		default:
			return mockResponse(200, `{}`), nil
		}
	}}
	if err := real.Login("user@example.com", "pw", nil); err != nil {
		t.Fatalf("login: %v", err)
	}
	return requests
}

func accountLoginRequest(t *testing.T, requests []recordedRequest) recordedRequest {
	t.Helper()
	for _, r := range requests {
		if strings.HasSuffix(r.path, "/accountLogin") {
			return r
		}
	}
	t.Fatalf("no accountLogin request in %v", requests)
	return recordedRequest{}
}

func TestChinaLoginUsesChinaAuthDomainAndCountry(t *testing.T) {
	requests := runMockLogin(t, "icloud.com.cn", map[string]string{"X-Apple-ID-Account-Country": "CHN"})
	for _, r := range requests {
		if strings.HasPrefix(r.path, "/appleauth/") && r.host != "idmsa.apple.com.cn" {
			t.Fatalf("auth request %s went to %s, want idmsa.apple.com.cn", r.path, r.host)
		}
	}
	start, _ := url.ParseQuery(requests[0].query)
	if got := start.Get("redirect_uri"); got != "https://www.icloud.com.cn" {
		t.Fatalf("redirect_uri = %q, want https://www.icloud.com.cn", got)
	}
	login := accountLoginRequest(t, requests)
	if login.host != "setup.icloud.com.cn" {
		t.Fatalf("accountLogin host = %s, want setup.icloud.com.cn", login.host)
	}
	if login.body["accountCountryCode"] != "CHN" {
		t.Fatalf("accountCountryCode = %v, want CHN", login.body["accountCountryCode"])
	}
	if login.body["dsWebAuthToken"] != "session-tok" || login.body["trustToken"] != "trust-tok" {
		t.Fatalf("accountLogin tokens not forwarded: %v", login.body)
	}
}

func TestChinaLoginFallsBackToCHNWithoutCountryHeader(t *testing.T) {
	login := accountLoginRequest(t, runMockLogin(t, "icloud.com.cn", nil))
	if login.body["accountCountryCode"] != "CHN" {
		t.Fatalf("accountCountryCode = %v, want CHN fallback", login.body["accountCountryCode"])
	}
}

func TestGlobalLoginKeepsGlobalDomainAndCountryHeader(t *testing.T) {
	requests := runMockLogin(t, "icloud.com", map[string]string{"X-Apple-ID-Account-Country": "GBR"})
	for _, r := range requests {
		if strings.HasPrefix(r.path, "/appleauth/") && r.host != "idmsa.apple.com" {
			t.Fatalf("auth request %s went to %s, want idmsa.apple.com", r.path, r.host)
		}
	}
	start, _ := url.ParseQuery(requests[0].query)
	if got := start.Get("redirect_uri"); got != "https://www.icloud.com" {
		t.Fatalf("redirect_uri = %q", got)
	}
	login := accountLoginRequest(t, requests)
	if login.host != "setup.icloud.com" || login.body["accountCountryCode"] != "GBR" {
		t.Fatalf("accountLogin host=%s country=%v", login.host, login.body["accountCountryCode"])
	}
}

func TestLoginRemembersAccountCountry(t *testing.T) {
	real, err := NewClient(nil, "icloud.com.cn", "", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(real.httpc.CloseIdleConnections)
	real.httpc = mockHTTP{HttpClient: real.httpc, do: func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/signin/init"):
			return mockResponse(200, `{"iteration":1,"salt":"AQ==","b":"Ag==","c":"c","protocol":"s2k"}`), nil
		case strings.HasSuffix(req.URL.Path, "/2sv/trust"):
			resp := mockResponse(204, ``)
			resp.Header.Set("X-Apple-Session-Token", "s")
			resp.Header.Set("X-Apple-ID-Account-Country", "CHN")
			return resp, nil
		case strings.HasSuffix(req.URL.Path, "/accountLogin"):
			return accountLoginResp(), nil
		}
		return mockResponse(200, `{}`), nil
	}}
	if err := real.Login("user@example.com", "pw", nil); err != nil {
		t.Fatal(err)
	}
	if real.AccountCountry() != "CHN" {
		t.Fatalf("AccountCountry = %q, want CHN", real.AccountCountry())
	}
}

func TestRefreshWebSessionUsesRegionAndStoredCountry(t *testing.T) {
	for _, tc := range []struct {
		host, stored, wantHost, wantCountry string
	}{
		{"icloud.com.cn", "", "setup.icloud.com.cn", "CHN"},
		{"icloud.com.cn", "HKG", "setup.icloud.com.cn", "HKG"},
		{"icloud.com", "", "setup.icloud.com", "USA"},
	} {
		var got recordedRequest
		c := &Client{Host: tc.host, httpc: mockHTTP{do: func(req *http.Request) (*http.Response, error) {
			got = recordedRequest{host: req.URL.Host, path: req.URL.Path}
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &got.body)
			return accountLoginResp(), nil
		}}}
		c.SetAccountCountry(tc.stored)
		if err := c.RefreshWebSession("tok", "trust"); err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		if got.host != tc.wantHost || got.path != "/setup/ws/1/accountLogin" || got.body["accountCountryCode"] != tc.wantCountry {
			t.Fatalf("%+v: sent host=%s path=%s country=%v", tc, got.host, got.path, got.body["accountCountryCode"])
		}
	}
}

// 中国区 accountLogin 在 setup.icloud.com.cn 设置 Cookie。tls-client 的 jar 对 4 段主机名
// 按完整主机分组,按 www.icloud.com.cn 读不到;登录必须直接用响应里的 Set-Cookie。
func TestChinaLoginKeepsCookiesFromAccountLoginResponse(t *testing.T) {
	c, err := NewClient(nil, "icloud.com.cn", "", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.httpc.CloseIdleConnections)
	c.httpc = mockHTTP{HttpClient: c.httpc, do: func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/signin/init"):
			return mockResponse(200, `{"iteration":1,"salt":"AQ==","b":"Ag==","c":"c","protocol":"s2k"}`), nil
		case strings.HasSuffix(req.URL.Path, "/2sv/trust"):
			resp := mockResponse(204, ``)
			resp.Header.Set("X-Apple-Session-Token", "s")
			return resp, nil
		case strings.HasSuffix(req.URL.Path, "/accountLogin"):
			if req.URL.Host != "setup.icloud.com.cn" {
				t.Errorf("accountLogin host = %s", req.URL.Host)
			}
			resp := accountLoginResp()
			resp.Header.Add("Set-Cookie", "X-APPLE-WEBAUTH-USER=u; Path=/")
			return resp, nil
		}
		return mockResponse(200, `{}`), nil
	}}
	if err := c.Login("user@example.com", "pw", nil); err != nil {
		t.Fatal(err)
	}
	if c.Cookies["X-APPLE-WEBAUTH-TOKEN"] != "fresh" || c.Cookies["X-APPLE-WEBAUTH-USER"] != "u" {
		t.Fatalf("cookies from accountLogin were lost: %v", c.Cookies)
	}
}

func TestLoginFailsClearlyWhenAccountLoginSetsNoWebAuthCookie(t *testing.T) {
	c, err := NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.httpc.CloseIdleConnections)
	c.httpc = mockHTTP{HttpClient: c.httpc, do: func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/signin/init"):
			return mockResponse(200, `{"iteration":1,"salt":"AQ==","b":"Ag==","c":"c","protocol":"s2k"}`), nil
		case strings.HasSuffix(req.URL.Path, "/2sv/trust"):
			resp := mockResponse(204, ``)
			resp.Header.Set("X-Apple-Session-Token", "s")
			return resp, nil
		}
		return mockResponse(200, `{}`), nil
	}}
	err = c.Login("user@example.com", "pw", nil)
	if err == nil || !strings.Contains(err.Error(), "X-APPLE-WEBAUTH-TOKEN") {
		t.Fatalf("want a missing-cookie error, got %v", err)
	}
}

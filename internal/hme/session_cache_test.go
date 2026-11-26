package hme

import (
	"testing"

	http "github.com/bogdanfinn/fhttp"
)

const listOK = `{"success":true,"result":{"hmeEmails":[]}}`

func TestReusedSessionSkipsValidate(t *testing.T) {
	var paths []string
	c := &Client{Cookies: map[string]string{"session": "v"}, httpc: mockHTTP{do: func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Host+req.URL.Path)
		return mockResponse(200, listOK), nil
	}}}
	c.UseSession(Session{ServiceURL: "https://p68-maildomainws.icloud.com", DSID: "42", ClientID: "cid"})
	if _, err := c.ListAliases(); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "p68-maildomainws.icloud.com/v2/hme/list" {
		t.Fatalf("reused session must call list directly, got %v", paths)
	}
}

func TestRejectedReusedSessionRevalidatesOnce(t *testing.T) {
	var paths []string
	var stored Session
	c := &Client{Cookies: map[string]string{"session": "v"}, httpc: mockHTTP{do: func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Host+req.URL.Path)
		switch req.URL.Host + req.URL.Path {
		case "p1-maildomainws.icloud.com/v2/hme/list":
			return mockResponse(401, `{}`), nil
		case "setup.icloud.com/setup/ws/1/validate":
			return mockResponse(200, `{"webservices":{"premiummailsettings":{"url":"https://p2-maildomainws.icloud.com:443"}},"dsInfo":{"dsid":"7"}}`), nil
		case "p2-maildomainws.icloud.com/v2/hme/list":
			return mockResponse(200, listOK), nil
		}
		t.Fatalf("unexpected request %s", req.URL)
		return nil, nil
	}}}
	c.UseSession(Session{ServiceURL: "https://p1-maildomainws.icloud.com", DSID: "1"})
	c.OnValidated = func(s Session) { stored = s }
	if _, err := c.ListAliases(); err != nil {
		t.Fatal(err)
	}
	want := []string{"p1-maildomainws.icloud.com/v2/hme/list", "setup.icloud.com/setup/ws/1/validate", "p2-maildomainws.icloud.com/v2/hme/list"}
	if len(paths) != len(want) {
		t.Fatalf("requests %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("requests %v, want %v", paths, want)
		}
	}
	if stored.ServiceURL != "https://p2-maildomainws.icloud.com" || stored.DSID != "7" {
		t.Fatalf("revalidated session not reported: %+v", stored)
	}
}

func TestFreshSessionAuthErrorIsNotRetried(t *testing.T) {
	calls := 0
	c := &Client{serviceURL: "https://p1-maildomainws.icloud.com", httpc: mockHTTP{do: func(req *http.Request) (*http.Response, error) {
		calls++
		return mockResponse(401, `{}`), nil
	}}}
	if _, err := c.ListAliases(); !IsAuthenticationError(err) {
		t.Fatalf("expected auth error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("a session validated by this client must not revalidate, got %d requests", calls)
	}
}

func TestSessionClientsShareTransportWithoutJarSwap(t *testing.T) {
	transport, err := NewTransport("")
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	a := NewSessionClient(map[string]string{"s": "1"}, "icloud.com.cn", false, transport)
	b := NewSessionClient(map[string]string{"s": "2"}, "", false, transport)
	if a.httpc != b.httpc || !a.sharedTransport {
		t.Fatal("session clients must share the transport")
	}
	if a.Host != "icloud.com.cn" || b.Host != "icloud.com" {
		t.Fatalf("unexpected hosts %q %q", a.Host, b.Host)
	}
	if transport.httpc.GetCookieJar() != nil {
		t.Fatal("shared transport must not carry a cookie jar")
	}
}

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icloud-hme/internal/hme"
	icloudmail "icloud-hme/internal/mail"
)

func TestFixedPickupAddress(t *testing.T) {
	srv := newCreateJobTestServer(t)
	account, err := srv.mgr.AddAccount("Pickup test", "", "icloud.com", "")
	if err != nil {
		t.Fatal(err)
	}
	srv.SetAPIToken("pickup-test-token")
	dir := t.TempDir()
	if err := srv.OpenPickupStore(dir); err != nil {
		t.Fatal(err)
	}
	if err := srv.SetPublicURL("https://icloud.example/"); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string, authorized bool) *httptest.ResponseRecorder {
		req := localRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-Host", "untrusted.example")
		if authorized {
			req.Header.Set("Authorization", "Bearer pickup-test-token")
		}
		response := httptest.NewRecorder()
		srv.Handler().ServeHTTP(response, req)
		return response
	}
	decode := func(response *httptest.ResponseRecorder) map[string]json.RawMessage {
		t.Helper()
		if response.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", response.Code, response.Body)
		}
		var payload struct{ Data map[string]json.RawMessage }
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload.Data
	}
	lookup := "/api/pickup-address?" + url.Values{"account_id": {account.ID}, "alias": {"Alias@Example.com"}}.Encode()
	const prefix = "https://icloud.example"
	path := "/api/pickup/" + account.ID + "/alias@example.com"
	srv.setCachedAliases(account.ID, []hme.Alias{{Email: "alias@example.com", Active: true}, {Email: "second@example.com", Active: true}})
	if response := request(http.MethodGet, lookup, "", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", response.Code)
	}
	if response := request(http.MethodGet, path, "", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", response.Code)
	}
	expected := string(decode(request(http.MethodGet, lookup, "", true))["pickup_url"])
	secret := strings.TrimSuffix(strings.TrimPrefix(expected, `"`+prefix+"/api/pickup/"), `"`)
	if !validPickupSecret(secret) {
		t.Fatal("expected an opaque random URL")
	}
	if info, err := os.Stat(filepath.Join(dir, "pickup_links.json")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("expected private on-disk link store: %v %v", info, err)
	}
	if err := srv.OpenPickupStore(dir); err != nil {
		t.Fatal(err)
	}
	if got := string(decode(request(http.MethodGet, lookup, "", true))["pickup_url"]); got != expected {
		t.Fatal("URL changed after reopening the store")
	}
	publicPath := "/api/pickup/" + secret
	if response := request(http.MethodGet, publicPath+"?alias=other@example.com&account_id=other", "", false); response.Code != http.StatusBadGateway || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("valid link should reach the scoped mailbox (unconfigured in test), got %d", response.Code)
	}
	if response := request(http.MethodGet, "/api/pickup/"+strings.Repeat("0", 64), "", false); response.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown link, got %d", response.Code)
	}
	for _, query := range []string{"", "?format=json"} {
		req := localRequest(http.MethodGet, publicPath+query, nil)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
		response := httptest.NewRecorder()
		srv.Handler().ServeHTTP(response, req)
		want := "text/html"
		if query != "" {
			want = "application/json"
		}
		if !strings.HasPrefix(response.Header().Get("Content-Type"), want) || response.Code != http.StatusBadGateway {
			t.Fatalf("expected %s error response, got %d %s", want, response.Code, response.Header().Get("Content-Type"))
		}
	}
	var aliases []aliasWithPickup
	if err := json.Unmarshal(decode(request(http.MethodGet, "/api/aliases?account_id="+account.ID, "", true))["aliases"], &aliases); err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 2 || `"`+aliases[0].PickupURL+`"` != expected || aliases[0].PickupURL == aliases[1].PickupURL {
		t.Fatal("expected one distinct stable URL per alias")
	}
	body := `{"account_id":"` + account.ID + `","label":"Pickup"}`
	if got := string(decode(request(http.MethodPost, "/api/create", body, true))["pickup_url"]); got != expected {
		t.Fatalf("create returned %s, want %s", got, expected)
	}
	var batch []createdWithPickup
	if err := json.Unmarshal(decode(request(http.MethodPost, "/api/create/batch", `{"account_id":"`+account.ID+`","count":1}`, true))["created"], &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch) != 1 || `"`+batch[0].PickupURL+`"` != expected {
		t.Fatalf("unexpected batch: %+v", batch)
	}
	if response := request(http.MethodGet, "/api/pickup/missing/alias@example.com?account_id="+account.ID, "", true); response.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown path account, got %d", response.Code)
	}
	if response := request(http.MethodGet, "/api/pickup/"+account.ID+"/invalid?alias=alias@example.com", "", true); response.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid path alias, got %d", response.Code)
	}
	srv.setCachedAliases(account.ID, []hme.Alias{{Email: "alias@example.com", Active: true}})
	rotate := `{"account_id":"` + account.ID + `","alias":"alias@example.com"}`
	if response := request(http.MethodPost, "/api/pickup-address/rotate", rotate, false); response.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthenticated rotation to fail, got %d", response.Code)
	}
	if got := string(decode(request(http.MethodPost, "/api/pickup-address/rotate", rotate, true))["pickup_url"]); got == expected {
		t.Fatal("rotation did not change the pickup address")
	}
	if response := request(http.MethodGet, publicPath, "", false); response.Code != http.StatusNotFound {
		t.Fatalf("expected revoked pickup URL to return 404, got %d", response.Code)
	}
}

func TestPickupPageEscapesMailAndPreservesFilters(t *testing.T) {
	var output bytes.Buffer
	err := pickupPageTemplate.Execute(&output, pickupPageData{
		Alias: "example@icloud.com", Limit: 1, Days: 7,
		Messages: []icloudmail.Message{{
			From: "sender@example.org", To: "private@example.net",
			Subject: "<script>test</script>", Preview: "First line\nexample@icloud.com",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	page := output.String()
	for _, expected := range []string{
		`name="limit" min="1" max="30" value="1"`, `name="days" value="7"`, "&lt;script&gt;test&lt;/script&gt;",
		`<dt>发件人</dt><dd><!--email_off-->sender@example.org<!--/email_off--></dd>`,
		`<dt>收件人</dt><dd><!--email_off-->example@icloud.com<!--/email_off--></dd>`,
		"<pre><!--email_off-->First line\nexample@icloud.com<!--/email_off--></pre>",
	} {
		if !strings.Contains(page, expected) {
			t.Errorf("page does not contain %q", expected)
		}
	}
	if strings.Contains(page, "private@example.net") {
		t.Fatal("pickup page exposes the forwarded-to mailbox")
	}
}

func TestPickupPageShowsUTC8Time(t *testing.T) {
	var output bytes.Buffer
	err := pickupPageTemplate.Execute(&output, pickupPageData{
		Alias: "example@icloud.com", Limit: 1, Days: 7,
		Messages: []icloudmail.Message{{Subject: "Code", Date: "2026-11-25T07:52:32Z"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `<time datetime="2026-11-25T07:52:32Z">2026-11-25 15:52:32（UTC+8）</time>`
	if !strings.Contains(output.String(), want) {
		t.Fatalf("pickup page time not shown in UTC+8; want %q in:\n%s", want, output.String())
	}
}

package hme

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
)

type smsRecord struct {
	method, path string
	body         map[string]any
}

// smsMock answers a 2FA login whose options list the given phones JSON.
func smsMock(t *testing.T, options string) (*Client, *[]smsRecord) {
	t.Helper()
	c, err := NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.httpc.CloseIdleConnections)
	var recs []smsRecord
	c.httpc = mockHTTP{HttpClient: c.httpc, do: func(req *http.Request) (*http.Response, error) {
		r := smsRecord{method: req.Method, path: req.URL.Path}
		if req.Body != nil {
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &r.body)
		}
		recs = append(recs, r)
		p := req.URL.Path
		switch {
		case strings.HasSuffix(p, "/signin/init"):
			return mockResponse(200, `{"iteration":1,"salt":"AQ==","b":"Ag==","c":"c","protocol":"s2k"}`), nil
		case strings.HasSuffix(p, "/signin/complete"):
			resp := mockResponse(409, `{}`)
			resp.Header.Set("scnt", "scnt-1")
			resp.Header.Set("X-Apple-ID-Session-Id", "sid-1")
			return resp, nil
		case p == "/appleauth/auth" && req.Method == "GET":
			return mockResponse(200, options), nil
		case p == "/appleauth/auth/verify/phone" && req.Method == "PUT":
			return mockResponse(200, `{}`), nil
		case strings.HasSuffix(p, "/securitycode"):
			resp := mockResponse(200, `{}`)
			resp.Header.Set("X-Apple-Session-Token", "sess")
			return resp, nil
		case strings.HasSuffix(p, "/accountLogin"):
			return accountLoginResp(), nil
		}
		return mockResponse(200, `{}`), nil
	}}
	return c, &recs
}

func findRec(recs []smsRecord, method, path string) (smsRecord, bool) {
	for _, r := range recs {
		if r.method == method && r.path == path {
			return r, true
		}
	}
	return smsRecord{}, false
}

func TestRequestSMSCodeThenCompleteUsesPhoneVerification(t *testing.T) {
	c, recs := smsMock(t, `{"trustedPhoneNumbers":[{"id":3,"obfuscatedNumber":"••• ••12","pushMode":"sms"}]}`)
	if _, err := c.LoginBegin("user@example.com", "pw"); err != nil {
		t.Fatal(err)
	}
	phone, err := c.RequestSMSCode()
	if err != nil || phone != "••• ••12" {
		t.Fatalf("RequestSMSCode phone=%q err=%v", phone, err)
	}
	put, ok := findRec(*recs, "PUT", "/appleauth/auth/verify/phone")
	if !ok || put.body["mode"] != "sms" || put.body["phoneNumber"].(map[string]any)["id"] != float64(3) {
		t.Fatalf("SMS request not sent as expected: %+v", put)
	}
	if err := c.CompleteOTP("123456"); err != nil {
		t.Fatal(err)
	}
	verify, ok := findRec(*recs, "POST", "/appleauth/auth/verify/phone/securitycode")
	if !ok {
		t.Fatalf("SMS code must be verified via verify/phone/securitycode, got %+v", *recs)
	}
	if verify.body["mode"] != "sms" || verify.body["phoneNumber"].(map[string]any)["id"] != float64(3) {
		t.Fatalf("verify body = %v", verify.body)
	}
	if _, ok := findRec(*recs, "POST", "/appleauth/auth/verify/trusteddevice/securitycode"); ok {
		t.Fatal("SMS code must not be sent to the trusted-device endpoint")
	}
}

func TestRequestSMSCodePrefersDefaultPhone(t *testing.T) {
	c, recs := smsMock(t, `{"trustedPhoneNumbers":[{"id":1,"obfuscatedNumber":"a"},{"id":2,"obfuscatedNumber":"b"}],"phoneNumberVerification":{"trustedPhoneNumber":{"id":2,"obfuscatedNumber":"b"}}}`)
	if _, err := c.LoginBegin("user@example.com", "pw"); err != nil {
		t.Fatal(err)
	}
	if phone, err := c.RequestSMSCode(); err != nil || phone != "b" {
		t.Fatalf("phone=%q err=%v", phone, err)
	}
	put, _ := findRec(*recs, "PUT", "/appleauth/auth/verify/phone")
	if put.body["phoneNumber"].(map[string]any)["id"] != float64(2) {
		t.Fatalf("expected default phone id 2, got %v", put.body)
	}
}

func TestRequestSMSCodeWithoutPhone(t *testing.T) {
	c, _ := smsMock(t, `{"trustedDevices":[]}`)
	if _, err := c.LoginBegin("user@example.com", "pw"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RequestSMSCode(); !errors.Is(err, ErrNoTrustedPhone) {
		t.Fatalf("want ErrNoTrustedPhone, got %v", err)
	}
	if !c.NeedsOTP() {
		t.Fatal("handshake must survive a failed SMS request")
	}
}

func TestRequestSMSCodeWithoutPendingLogin(t *testing.T) {
	c, _ := smsMock(t, `{}`)
	if _, err := c.RequestSMSCode(); err == nil {
		t.Fatal("expected error without a pending handshake")
	}
}

// Package hme - iCloud 认证模块
//
// 基于 Go-iClient 项目实现完整的 SRP (Secure Remote Password) 登录流程,
// 支持双重认证 (2FA),登录成功后提取 session token Cookie。
package hme

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/pbkdf2"

	http "github.com/bogdanfinn/fhttp"
	"icloud-hme/internal/srp"
)

// AuthEndpoints iCloud 认证 API 端点
const (
	OAuthClientID = "d39ba9916b7251055b22c7f910e2ea796ee65e98b2ddecea8f5dde8d9d1a815d"

	// 以下是认证域名(见 authRoot)下的路径,按账号区域拼成完整地址。
	authStartPathFmt   = "/appleauth/auth/authorize/signin?frame_id=auth-%s&language=en_US&skVersion=7&iframeId=auth-%s&client_id=%s&redirect_uri=%s&response_type=code&response_mode=web_message&state=auth-%s&authVersion=latest"
	authFederatePath   = "/appleauth/auth/federate?isRememberMeEnabled=true"
	authInitPath       = "/appleauth/auth/signin/init"
	authCompletePath   = "/appleauth/auth/signin/complete?isRememberMeEnabled=true"
	submitSecurityPath = "/appleauth/auth/verify/%s/securitycode"
	authOptionsPath    = "/appleauth/auth"
	verifyPhonePath    = "/appleauth/auth/verify/phone"
	authTrustPath      = "/appleauth/auth/2sv/trust"

	accountCountryHeader = "X-Apple-ID-Account-Country"
)

// authRoot 返回 Apple ID 认证域名。中国大陆账号(icloud.com.cn)必须用 idmsa.apple.com.cn,
// 与 pyicloud 的 cn 分支一致;用全球域名登录再调 setup.icloud.com.cn 的 accountLogin 会返回 400。
func (c *Client) authRoot() string {
	if c.Host == "icloud.com.cn" {
		return "https://idmsa.apple.com.cn"
	}
	return "https://idmsa.apple.com"
}

func (c *Client) authURL(path string) string { return c.authRoot() + path }

// accountCountryOr 返回 accountLogin 用的国家代码:优先用 Apple 在认证响应头里给的值,
// 没拿到时按区域推断(中国大陆 CHN,其它 USA)。
func (c *Client) accountCountryOr(country string) string {
	if country != "" {
		return country
	}
	if c.Host == "icloud.com.cn" {
		return "CHN"
	}
	return "USA"
}

func accountLoginBody(sessionToken, trustToken, country string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"dsWebAuthToken":     sessionToken,
		"accountCountryCode": country,
		"extended_login":     true,
		"trustToken":         trustToken,
	})
}

// captureAuthHeaders 从任意认证响应头读取会话状态,与 pyicloud 的 HEADER_DATA 一致。
// Apple 不保证每个响应都带全部字段,所以只在值非空时覆盖,避免后面的响应把已拿到的
// session token 清空(之前 getTrust 无条件赋值,trust 响应不带 token 时 accountLogin 会 400)。
func captureAuthHeaders(state *authState, resp *http.Response) {
	set := func(dst *string, header string) {
		if v := resp.Header.Get(header); v != "" {
			*dst = v
		}
	}
	set(&state.accountCountry, accountCountryHeader)
	set(&state.authToken, "X-Apple-Session-Token")
	set(&state.trustToken, "X-Apple-TwoSV-Trust-Token")
	set(&state.scnt, "scnt")
	set(&state.sessionID, "X-Apple-ID-Session-Id")
}

// OTPProvider 双重认证回调函数,返回 2FA 验证码
type OTPProvider func() (string, error)

var (
	ErrInvalidAppleCredentials = errors.New("Apple ID 或密码错误，请使用 Apple 账号登录密码")
	ErrAppleVerificationNeeded = errors.New("请输入受信任 Apple 设备上的 6 位验证码")
	ErrInvalidAppleCode        = errors.New("Apple 未接受此验证码，请获取新的 6 位验证码后重试")
	ErrAppleTermsRequired      = errors.New("请先在 Apple 账号网页确认隐私条款")
)

// authState 保存认证过程中的状态
type authState struct {
	username   string
	password   string
	frameId    string
	clientId   string
	challenge  string
	authAttr   string
	sessionID  string
	scnt       string
	authToken  string
	trustToken string
	dsid       string

	accountCountry string // X-Apple-ID-Account-Country,accountLogin 需要

	// trustHeaderNames 记录 2sv/trust 响应里 X-Apple-* 头的名字(不含值),
	// 用于 Apple 没下发 session token 时定位原因。
	trustHeaderNames string

	// smsPhoneID 非 0 表示已请求短信验证码,验证码要走 verify/phone/securitycode 校验。
	smsPhoneID int

	// webCookies 是 accountLogin 响应 Set-Cookie 里的 iCloud Web Cookie。
	webCookies map[string]string
}

// ErrNoTrustedPhone 表示账号没有可接收短信验证码的受信任手机号。
var ErrNoTrustedPhone = errors.New("这个 Apple 账号没有可接收短信的受信任手机号")

type trustedPhone struct {
	ID               int    `json:"id"`
	ObfuscatedNumber string `json:"obfuscatedNumber"`
	PushMode         string `json:"pushMode"`
}

// Login 使用 iCloud 账号密码登录,获取 session token Cookie。
//
// 这是 LoginBegin + CompleteOTP 的一步式封装:otpProvider 为 nil 且账号需要 2FA 时,
// 返回 ErrAppleVerificationNeeded。两步式登录(验证码单独提交)请直接用这两个方法。
func (c *Client) Login(username, password string, otpProvider OTPProvider) error {
	needsOTP, err := c.LoginBegin(username, password)
	if err != nil {
		return err
	}
	if needsOTP {
		if otpProvider == nil {
			return ErrAppleVerificationNeeded
		}
		otp, err := otpProvider()
		if err != nil {
			return fmt.Errorf("获取 2FA 验证码失败: %w", err)
		}
		return c.CompleteOTP(otp)
	}
	return c.finishWebLogin()
}

// LoginBegin 执行登录握手直到 Apple 要求 2FA 为止。
//
// 返回 needsOTP=true 表示 Apple 已向受信任设备推送验证码,调用方应随后在**同一个
// Client** 上调用 CompleteOTP(code),使验证码对应这次握手。不要新建 Client 重来,
// 否则 Apple 会把它当成新一次登录并重新推送验证码。
//
// 返回 needsOTP=false 表示账号无需 2FA,握手已到可收尾状态,调用方应调用 finishWebLogin。
func (c *Client) LoginBegin(username, password string) (needsOTP bool, err error) {
	state := &authState{username: username, password: password}
	c.pending = nil

	if err := c.authStart(state); err != nil {
		return false, fmt.Errorf("auth start: %w", err)
	}
	if err := c.authFederate(state); err != nil {
		return false, fmt.Errorf("auth federate: %w", err)
	}

	params := srp.GetParams(2048)
	params.NoUserNameInX = true
	srpClient := srp.NewSRPClient(params, nil)

	authInitResp, err := c.authInit(state, base64.StdEncoding.EncodeToString(srpClient.GetABytes()))
	if err != nil {
		return false, fmt.Errorf("auth init: %w", err)
	}
	state.challenge = authInitResp.C

	bDec, err := base64.StdEncoding.DecodeString(authInitResp.B)
	if err != nil {
		return false, fmt.Errorf("decode B: %w", err)
	}
	saltDec, err := base64.StdEncoding.DecodeString(authInitResp.Salt)
	if err != nil {
		return false, fmt.Errorf("decode salt: %w", err)
	}

	passHash := sha256.Sum256([]byte(password))
	passwordInput := passHash[:]
	if authInitResp.Protocol == "s2k_fo" {
		passwordInput = []byte(hex.EncodeToString(passwordInput))
	}
	passKey := pbkdf2.Key(passwordInput, saltDec, authInitResp.Iteration, 32, sha256.New)

	if err := srpClient.ProcessClientChanllenge([]byte(username), passKey, saltDec, bDec); err != nil {
		return false, fmt.Errorf("invalid SRP challenge: %w", err)
	}

	needsOTP, err = c.authComplete(state, base64.StdEncoding.EncodeToString(srpClient.M1), base64.StdEncoding.EncodeToString(srpClient.M2))
	if err != nil {
		return false, fmt.Errorf("auth complete: %w", err)
	}
	c.pending = state
	return needsOTP, nil
}

// CompleteOTP 在 LoginBegin 保留的握手上提交 2FA 验证码并收尾。
func (c *Client) CompleteOTP(code string) error {
	if c.pending == nil {
		return errors.New("没有待验证的登录会话,请重新用密码登录")
	}
	if err := c.submitSecurityCode(c.pending, code); err != nil {
		return err
	}
	return c.finishWebLogin()
}

// RequestSMSCode 在待验证的握手上请求 Apple 把验证码以短信发到受信任手机号,
// 返回脱敏的号码(例如 "••• •••• ••12")。之后用 CompleteOTP 提交短信里的验证码。
//
// 较新的账号只通过 websocket "bridge" 流程向设备推送验证码,本客户端没实现那套流程;
// 短信是 pyicloud 在 bridge 失败时使用的备用路径。
func (c *Client) RequestSMSCode() (string, error) {
	state := c.pending
	if state == nil {
		return "", errors.New("没有待验证的登录会话,请重新用密码登录")
	}
	phone, err := c.defaultTrustedPhone(state)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(map[string]any{
		"phoneNumber": map[string]any{"id": phone.ID},
		"mode":        "sms",
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest("PUT", c.authURL(verifyPhonePath), bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header = c.updateAuthHeaders(req.Header, state)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	captureAuthHeaders(state, resp)
	if resp.StatusCode != 200 && resp.StatusCode != 202 && resp.StatusCode != 204 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return "", fmt.Errorf("请求短信验证码失败: HTTP %d%s", resp.StatusCode, appleErrorSummary(raw))
	}
	state.smsPhoneID = phone.ID
	return phone.ObfuscatedNumber, nil
}

// defaultTrustedPhone 读取 2FA 选项,返回 Apple 标出的默认受信任手机号(没有则取第一个)。
func (c *Client) defaultTrustedPhone(state *authState) (trustedPhone, error) {
	req, err := http.NewRequest("GET", c.authURL(authOptionsPath), nil)
	if err != nil {
		return trustedPhone{}, err
	}
	req.Header = c.updateAuthHeaders(req.Header, state)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return trustedPhone{}, err
	}
	defer resp.Body.Close()
	captureAuthHeaders(state, resp)
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	// Apple 对这个 GET 可能回 200 或 423/409,只要 body 里有手机号就能用。
	var opts struct {
		TrustedPhoneNumber  *trustedPhone  `json:"trustedPhoneNumber"`
		TrustedPhoneNumbers []trustedPhone `json:"trustedPhoneNumbers"`
		PhoneVerification   struct {
			TrustedPhoneNumber  *trustedPhone  `json:"trustedPhoneNumber"`
			TrustedPhoneNumbers []trustedPhone `json:"trustedPhoneNumbers"`
		} `json:"phoneNumberVerification"`
	}
	if err := json.Unmarshal(raw, &opts); err != nil {
		return trustedPhone{}, fmt.Errorf("读取 2FA 选项失败: HTTP %d", resp.StatusCode)
	}
	for _, p := range []*trustedPhone{opts.TrustedPhoneNumber, opts.PhoneVerification.TrustedPhoneNumber} {
		if p != nil && p.ID != 0 {
			return *p, nil
		}
	}
	for _, list := range [][]trustedPhone{opts.TrustedPhoneNumbers, opts.PhoneVerification.TrustedPhoneNumbers} {
		for _, p := range list {
			if p.ID != 0 {
				return p, nil
			}
		}
	}
	return trustedPhone{}, ErrNoTrustedPhone
}

// NeedsOTP 返回当前 Client 是否有一个等待验证码的握手。
func (c *Client) NeedsOTP() bool { return c.pending != nil }

// finishWebLogin 完成设备信任、accountLogin 和 Cookie 提取,并保存会话 token。
func (c *Client) finishWebLogin() error {
	state := c.pending
	if state == nil {
		return errors.New("登录状态缺失")
	}
	if err := c.getTrust(state); err != nil {
		return fmt.Errorf("get trust: %w", err)
	}
	if err := c.authenticateWeb(state); err != nil {
		return fmt.Errorf("authenticate web: %w", err)
	}
	cookies := c.extractSessionCookies()
	for k, v := range state.webCookies {
		cookies[k] = v
	}
	if _, ok := cookies["X-APPLE-WEBAUTH-TOKEN"]; !ok {
		return fmt.Errorf("accountLogin 未返回 X-APPLE-WEBAUTH-TOKEN Cookie (收到 %d 个 Cookie)", len(state.webCookies))
	}
	c.Cookies = cookies
	c.sessionToken = state.authToken
	c.trustToken = state.trustToken
	c.accountCountry = c.accountCountryOr(state.accountCountry)
	c.pending = nil
	c.log("登录成功,获取到 %d 个 Cookie", len(cookies))
	return nil
}

// RefreshWebSession 用之前登录拿到的 session token 和 trust token 重新调用
// accountLogin,换取新的 iCloud Web Cookie,整个过程不需要密码和 2FA。
//
// 不依赖 Cookie jar:accountLogin 靠请求体里的 token 认证,新的 Cookie 直接从
// 响应的 Set-Cookie 读取,所以共享 transport(无 jar)上也能用。
//
// 仅在 session token 仍然有效时成功;token 过期后 Apple 会拒绝,此时只能重新登录。
func (c *Client) RefreshWebSession(sessionToken, trustToken string) error {
	if strings.TrimSpace(sessionToken) == "" {
		return errAutoReauthUnavailable
	}
	body, err := accountLoginBody(sessionToken, trustToken, c.accountCountryOr(c.accountCountry))
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", c.SetupURL()+"/accountLogin", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", c.Origin())
	req.Header.Set("Referer", c.Origin()+"/")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		// token 失效(通常 401/421)无法自动恢复,且响应体可能含凭据,不回传内容。
		return errAutoReauthUnavailable
	}

	cookies := make(map[string]string)
	for _, sc := range resp.Cookies() {
		if sc.Name != "" {
			cookies[sc.Name] = sc.Value
		}
	}
	if _, ok := cookies["X-APPLE-WEBAUTH-TOKEN"]; !ok {
		return errAutoReauthUnavailable
	}

	var result struct {
		DsInfo struct {
			Dsid string `json:"dsid"`
		} `json:"dsInfo"`
	}
	_ = json.Unmarshal(payload, &result)

	c.Cookies = cookies
	if result.DsInfo.Dsid != "" {
		c.dsid = result.DsInfo.Dsid
	}
	c.sessionToken = sessionToken
	c.trustToken = trustToken
	c.log("会话已免密刷新,获取到 %d 个 Cookie", len(cookies))
	return nil
}

var errAutoReauthUnavailable = errors.New("无法自动刷新会话,请重新登录 Apple ID")

// SessionToken 返回登录时获得的 session token(可能为空)。
func (c *Client) SessionToken() string { return c.sessionToken }

// TrustToken 返回登录时获得的 trust token(可能为空)。
func (c *Client) TrustToken() string { return c.trustToken }

// AccountCountry 返回登录时确定的账号国家代码(可能为空)。
func (c *Client) AccountCountry() string { return c.accountCountry }

// SetAccountCountry 设置免密刷新时使用的国家代码;为空时按区域推断。
func (c *Client) SetAccountCountry(country string) { c.accountCountry = country }

// --- 认证流程的各步骤 ---

// authStart 初始化 frameId 和 clientId
func (c *Client) authStart(state *authState) error {
	state.frameId = strings.ToLower(uuid.New().String())
	state.clientId = OAuthClientID

	req, err := http.NewRequest("GET", c.authURL(fmt.Sprintf(authStartPathFmt, state.frameId, state.frameId, state.clientId, c.Origin(), state.frameId)), nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	state.authAttr = resp.Header.Get("X-Apple-Auth-Attributes")
	return nil
}

// authFederate 提交用户名
func (c *Client) authFederate(state *authState) error {
	data, err := json.Marshal(map[string]any{"accountName": state.username, "rememberMe": true})
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", c.authURL(authFederatePath), bytes.NewReader(data))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header = c.updateAuthHeaders(req.Header, state)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	captureAuthHeaders(state, resp)

	if resp.StatusCode != 200 {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
	return nil
}

// authInitResp authInit 响应
type authInitResp struct {
	Iteration int    `json:"iteration"`
	Salt      string `json:"salt"`
	Protocol  string `json:"protocol"`
	B         string `json:"b"`
	C         string `json:"c"`
}

// authInit 初始化 SRP 认证
func (c *Client) authInit(state *authState, a string) (*authInitResp, error) {
	reqBody := map[string]interface{}{
		"a":           a,
		"accountName": state.username,
		"protocols":   []string{"s2k", "s2k_fo"},
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", c.authURL(authInitPath), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header = c.updateAuthHeaders(req.Header, state)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	captureAuthHeaders(state, resp)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth init failed: HTTP %d", resp.StatusCode)
	}
	var result authInitResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if result.Iteration <= 0 || result.Iteration > 1000000 || result.B == "" || result.Salt == "" || result.C == "" {
		return nil, fmt.Errorf("invalid auth init response: missing or invalid SRP challenge")
	}
	if result.Protocol != "s2k" && result.Protocol != "s2k_fo" {
		return nil, fmt.Errorf("unsupported SRP protocol: %q", result.Protocol)
	}
	return &result, nil
}

// authComplete 提交 SRP 响应
// authComplete 提交 SRP 响应。返回 needsOTP=true 表示 Apple 要求 2FA(HTTP 409)。
func (c *Client) authComplete(state *authState, m1, m2 string) (needsOTP bool, err error) {
	reqBody := map[string]interface{}{
		"accountName": state.username,
		"rememberMe":  true,
		"trustTokens": []string{},
		"m1":          m1,
		"c":           state.challenge,
		"m2":          m2,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return false, err
	}

	req, err := http.NewRequest("POST", c.authURL(authCompletePath), bytes.NewReader(data))
	if err != nil {
		return false, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header = c.updateAuthHeaders(req.Header, state)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	captureAuthHeaders(state, resp)

	switch resp.StatusCode {
	case 200:
		return false, nil
	case 409:
		return true, nil
	case 403:
		return false, ErrInvalidAppleCredentials
	case 412:
		return false, ErrAppleTermsRequired
	default:
		return false, fmt.Errorf("auth complete 失败: HTTP %d", resp.StatusCode)
	}
}

// submitSecurityCode 在已有握手(state)上提交 2FA 验证码。
func (c *Client) submitSecurityCode(state *authState, otp string) error {
	reqBody := map[string]interface{}{
		"securityCode": map[string]string{"code": otp},
	}
	kind := "trusteddevice"
	if state.smsPhoneID != 0 {
		kind = "phone"
		reqBody["phoneNumber"] = map[string]any{"id": state.smsPhoneID}
		reqBody["mode"] = "sms"
	}

	data, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", c.authURL(fmt.Sprintf(submitSecurityPath, kind)), bytes.NewReader(data))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header = c.updateAuthHeaders(req.Header, state)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	captureAuthHeaders(state, resp)

	switch resp.StatusCode {
	case 200, 204:
	case 400, 401, 403, 422:
		return ErrInvalidAppleCode
	default:
		return fmt.Errorf("2FA 验证失败: HTTP %d", resp.StatusCode)
	}

	return nil
}

// getTrust 获取 trust token
func (c *Client) getTrust(state *authState) error {
	req, err := http.NewRequest("GET", c.authURL(authTrustPath), nil)
	if err != nil {
		return err
	}

	req.Header = c.updateAuthHeaders(req.Header, state)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	captureAuthHeaders(state, resp)
	state.trustHeaderNames = fmt.Sprintf("HTTP %d; %s", resp.StatusCode, appleHeaderNames(resp.Header))

	if resp.StatusCode != 204 && resp.StatusCode != 200 {
		return fmt.Errorf("trust 失败: HTTP %d", resp.StatusCode)
	}
	return nil
}

// authenticateWeb 认证 iCloud Web 服务
func (c *Client) authenticateWeb(state *authState) error {
	if state.authToken == "" {
		return fmt.Errorf("Apple 未返回 session token (trust %s)", state.trustHeaderNames)
	}
	body, err := accountLoginBody(state.authToken, state.trustToken, c.accountCountryOr(state.accountCountry))
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", c.SetupURL()+"/accountLogin", bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", c.Origin())
	req.Header.Set("Referer", c.Origin()+"/")
	req.Header.Set("Accept", "*/*")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("auth web 失败: HTTP %d%s (country=%s, trust token=%t)",
			resp.StatusCode, appleErrorSummary(raw), c.accountCountryOr(state.accountCountry), state.trustToken != "")
	}

	// 直接从响应读 Cookie。tls-client 的 jar 按主机名的后两段分组,但只对 2~3 段的主机
	// 这样做:中国区 setup.icloud.com.cn 和 www.icloud.com.cn 各有 4 段,会落到不同分组,
	// 从 jar 按 Origin 读会读不到 accountLogin 设置的 Cookie。
	state.webCookies = make(map[string]string)
	for _, sc := range resp.Cookies() {
		if sc.Name != "" && sc.Value != "" {
			state.webCookies[sc.Name] = sc.Value
		}
	}

	var result struct {
		DsInfo struct {
			Dsid string `json:"dsid"`
		} `json:"dsInfo"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	state.dsid = result.DsInfo.Dsid

	// 复制认证域名的 Cookie 到 iCloud 域名
	u1, _ := url.Parse(c.authRoot())
	u2, _ := url.Parse("https://" + c.Host)
	cookies := c.httpc.GetCookies(u1)
	c.httpc.SetCookies(u2, cookies)

	return nil
}

// appleHeaderNames 返回响应里 X-Apple-* 和 scnt 头的名字(排序、去前缀、不含值)。
func appleHeaderNames(h http.Header) string {
	var names []string
	for k := range h {
		lk := strings.ToLower(k)
		switch {
		case strings.HasPrefix(lk, "x-apple-"):
			names = append(names, k[len("X-Apple-"):])
		case lk == "scnt":
			names = append(names, k)
		}
	}
	if len(names) == 0 {
		return "无 X-Apple 头"
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// extractSessionCookies 提取 session token Cookie
func (c *Client) extractSessionCookies() map[string]string {
	cookies := make(map[string]string)
	u, _ := url.Parse(c.Origin())
	for _, cookie := range c.httpc.GetCookies(u) {
		cookies[cookie.Name] = cookie.Value
	}
	return cookies
}

// updateAuthHeaders 更新认证请求所需的头部
func (c *Client) updateAuthHeaders(header http.Header, state *authState) http.Header {
	if state.scnt != "" {
		header.Set("scnt", state.scnt)
	}
	if state.sessionID != "" {
		header.Set("X-Apple-ID-Session-Id", state.sessionID)
	}

	// 与 pyicloud 的 _get_auth_headers 一致:缺少这些 OAuth/Widget 头时,Apple 接受验证码
	// 却不在 2sv/trust 响应里下发 X-Apple-Session-Token。
	header.Set("X-Apple-OAuth-Client-Id", OAuthClientID)
	header.Set("X-Apple-OAuth-Client-Type", "firstPartyAuth")
	header.Set("X-Apple-OAuth-Redirect-URI", c.Origin())
	header.Set("X-Apple-OAuth-Require-Grant-Code", "true")
	header.Set("X-Apple-OAuth-Response-Mode", "web_message")
	header.Set("X-Apple-OAuth-Response-Type", "code")
	header.Set("X-Apple-Widget-Key", OAuthClientID)
	if state.frameId != "" {
		header.Set("X-Apple-OAuth-State", "auth-"+state.frameId)
		header.Set("X-Apple-Frame-Id", "auth-"+state.frameId)
	}
	if state.authAttr != "" {
		header.Set("X-Apple-Auth-Attributes", state.authAttr)
	}
	header.Set("X-Requested-With", "XMLHttpRequest")
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/json")
	header.Set("Referer", c.Origin()+"/")
	header.Set("Origin", c.Origin())
	header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")

	return header
}

// Validate 验证当前 Cookie 是否有效
func (c *Client) Validate() (bool, error) {
	if len(c.Cookies) == 0 {
		return false, fmt.Errorf("无 Cookie")
	}
	// 简单实现：尝试调用 validate 端点
	err := c.ValidateSession()
	if err != nil {
		return false, err
	}
	return true, nil
}

// appleErrorSummary 只从 Apple 的错误响应里取出错误码和错误文本字段,
// 绝不回传原始响应体(里面可能有 trustTokens 等凭据)。
func appleErrorSummary(raw []byte) string {
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	var parts []string
	add := func(key string, v any) {
		switch x := v.(type) {
		case string:
			if x != "" {
				r := []rune(x)
				if len(r) > 80 {
					x = string(r[:80])
				}
				parts = append(parts, key+"="+x)
			}
		case float64:
			parts = append(parts, fmt.Sprintf("%s=%v", key, x))
		case bool:
			parts = append(parts, fmt.Sprintf("%s=%t", key, x))
		}
	}
	for _, key := range []string{"error", "errorCode", "errorMessage", "reason", "errorReason", "success"} {
		add(key, body[key])
	}
	if list, ok := body["serviceErrors"].([]any); ok {
		for i, item := range list {
			if i >= 3 {
				break
			}
			if m, ok := item.(map[string]any); ok {
				add("serviceError.code", m["code"])
				add("serviceError.message", m["message"])
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, " ") + "]"
}

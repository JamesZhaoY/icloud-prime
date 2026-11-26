// Package account 实现多账号管理器。
//
// 负责账号 CRUD、Cookie 解析(Header String / JSON)、持久化到 accounts.json,
// 以及创建 HME 客户端和邮件客户端。对应原 Python 项目 account_manager.py。
package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
)

// Account 描述一个 iCloud 账号。
type Account struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	RealEmail      string            `json:"real_email"`
	ICloudEmail    string            `json:"icloud_email"`
	Cookies        map[string]string `json:"cookies"`
	Host           string            `json:"host"`
	Proxy          string            `json:"proxy,omitempty"` // HTTP/SOCKS5 代理
	AppPassword    string            `json:"app_password,omitempty"`
	Status         string            `json:"status"`                  // active / error
	SessionToken   string            `json:"session_token,omitempty"` // 免密刷新会话用,敏感
	TrustToken     string            `json:"trust_token,omitempty"`   // 免密刷新会话用,敏感
	AccountCountry string            `json:"account_country,omitempty"`
	AliasTotal     int               `json:"alias_total"`
	AliasActive    int               `json:"alias_active"`
	LastValidated  string            `json:"last_validated"`
	LastError      string            `json:"last_error,omitempty"`
	CreatedAt      string            `json:"created_at"`
	HasCookies     bool              `json:"has_cookies"`
	HasAppPassword bool              `json:"has_app_password"`
}

// Manager 管理多个 iCloud 账号,线程安全。
type Manager struct {
	mu       sync.Mutex
	accounts map[string]*Account
	dataDir  string
	dataFile string

	// 网络资源缓存,与账号数据分开加锁,避免网络请求期间占用 mu。
	cacheMu       sync.Mutex
	sessions      map[string]sessionEntry
	transports    map[string]transportEntry
	pendingLogins map[string]pendingLogin
	imapPool      *mail.Pool
}

// hmeSessionTTL 是缓存 /validate 结果的最长时间;过期后下次请求重新校验。
const hmeSessionTTL = 30 * time.Minute

type sessionEntry struct {
	host    string
	session hme.Session
	expires time.Time
}

type transportEntry struct {
	proxy     string
	transport *hme.Transport
}

const statusReauthRequired = "reauth_required"

var ErrHMEReauthRequired = errors.New("iCloud 网页会话不可用，请重新登录 Apple ID 或更新 Cookie")

// NewManager 创建管理器。dataDir 用于存放 accounts.json。
func NewManager(dataDir string) (*Manager, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}
	m := &Manager{
		accounts:      make(map[string]*Account),
		dataDir:       dataDir,
		dataFile:      filepath.Join(dataDir, "accounts.json"),
		sessions:      make(map[string]sessionEntry),
		transports:    make(map[string]transportEntry),
		pendingLogins: make(map[string]pendingLogin),
		imapPool:      mail.NewPool(),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// Reload 重新加载 accounts.json 配置文件。
func (m *Manager) Reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resetNetworkCaches()
	return m.load()
}

// resetNetworkCaches 丢弃全部缓存的会话、HTTP 连接和空闲 IMAP 连接。
func (m *Manager) resetNetworkCaches() {
	m.cacheMu.Lock()
	transports := m.transports
	m.sessions = make(map[string]sessionEntry)
	m.transports = make(map[string]transportEntry)
	m.pendingLogins = make(map[string]pendingLogin)
	m.cacheMu.Unlock()
	for _, entry := range transports {
		entry.transport.Close()
	}
	if m.imapPool != nil {
		m.imapPool.CloseIdle()
	}
}

// forgetAccountNetwork 丢弃单个账号缓存的会话和 HTTP 连接。
func (m *Manager) forgetAccountNetwork(id string) {
	m.cacheMu.Lock()
	entry, hadTransport := m.transports[id]
	delete(m.sessions, id)
	delete(m.transports, id)
	m.cacheMu.Unlock()
	if hadTransport {
		entry.transport.Close()
	}
}

func (m *Manager) invalidateSession(id string) {
	m.cacheMu.Lock()
	delete(m.sessions, id)
	m.cacheMu.Unlock()
}

func (m *Manager) cachedSession(id, host string) (hme.Session, bool) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	entry, ok := m.sessions[id]
	if !ok || entry.host != host || time.Now().After(entry.expires) {
		return hme.Session{}, false
	}
	return entry.session, true
}

func (m *Manager) storeSession(id, host string, session hme.Session) {
	if session.ServiceURL == "" {
		return
	}
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if m.sessions == nil {
		m.sessions = make(map[string]sessionEntry)
	}
	m.sessions[id] = sessionEntry{host: host, session: session, expires: time.Now().Add(hmeSessionTTL)}
}

// transportFor 返回账号专用的共享 HTTP 客户端;代理变化时重建。
// 每个账号单独一个,避免不同账号的请求复用同一条 TLS 连接。
func (m *Manager) transportFor(id, proxy string) (*hme.Transport, error) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if m.transports == nil {
		m.transports = make(map[string]transportEntry)
	}
	if entry, ok := m.transports[id]; ok {
		if entry.proxy == proxy {
			return entry.transport, nil
		}
		entry.transport.Close()
		delete(m.sessions, id)
	}
	transport, err := hme.NewTransport(proxy)
	if err != nil {
		return nil, err
	}
	m.transports[id] = transportEntry{proxy: proxy, transport: transport}
	return transport, nil
}

func (m *Manager) load() error {
	raw, err := os.ReadFile(m.dataFile)
	if err != nil {
		if os.IsNotExist(err) {
			return m.importEditedExample()
		}
		return err
	}
	accounts, err := parseAccountsFile(raw)
	if err != nil {
		return err
	}
	m.accounts = accounts
	if m.accounts == nil {
		m.accounts = make(map[string]*Account)
	}
	return nil
}

func (m *Manager) importEditedExample() error {
	raw, err := os.ReadFile(filepath.Join(m.dataDir, "accounts.example.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	accounts, err := parseAccountsFile(raw)
	if err != nil {
		return err
	}
	configured := make(map[string]*Account)
	for _, acc := range accounts {
		if isEditedExampleAccount(acc) {
			configured[acc.ID] = acc
		}
	}
	if len(configured) == 0 {
		return nil
	}
	m.accounts = configured
	return m.save()
}

type accountDisk struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	RealEmail     string          `json:"real_email"`
	ICloudEmail   string          `json:"icloud_email"`
	Cookies       json.RawMessage `json:"cookies"`
	Host          string          `json:"host"`
	Proxy         string          `json:"proxy"`
	AppPassword   string          `json:"app_password"`
	AppPasswords  []appPassword   `json:"app_passwords"`
	Status        string          `json:"status"`
	AliasTotal    int             `json:"alias_total"`
	AliasActive   int             `json:"alias_active"`
	LastValidated string          `json:"last_validated"`
	LastError     string          `json:"last_error"`
	CreatedAt     string          `json:"created_at"`
}

type appPassword struct {
	ICloudEmail string `json:"icloud_email"`
	Password    string `json:"password"`
	AppPassword string `json:"app_password"`
}

type browserCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func parseAccountsFile(raw []byte) (map[string]*Account, error) {
	var wrapper struct {
		Accounts json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(wrapper.Accounts)) == 0 {
		return make(map[string]*Account), nil
	}

	accounts := make(map[string]*Account)
	switch firstJSONByte(wrapper.Accounts) {
	case '{':
		var byID map[string]json.RawMessage
		if err := json.Unmarshal(wrapper.Accounts, &byID); err != nil {
			return nil, err
		}
		for id, item := range byID {
			acc, err := parseAccount(id, item)
			if err != nil {
				return nil, err
			}
			accounts[acc.ID] = acc
		}
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(wrapper.Accounts, &items); err != nil {
			return nil, err
		}
		for _, item := range items {
			acc, err := parseAccount("", item)
			if err != nil {
				return nil, err
			}
			accounts[acc.ID] = acc
		}
	default:
		return nil, fmt.Errorf("accounts 必须是对象或数组")
	}
	return accounts, nil
}

func parseAccount(fallbackID string, raw json.RawMessage) (*Account, error) {
	var disk accountDisk
	if err := json.Unmarshal(raw, &disk); err != nil {
		return nil, err
	}
	if disk.ID == "" {
		disk.ID = fallbackID
	}
	if disk.ID == "" {
		return nil, fmt.Errorf("账号缺少 id")
	}

	cookies, err := parseCookiesJSON(disk.Cookies)
	if err != nil {
		return nil, err
	}
	if disk.AppPassword == "" && len(disk.AppPasswords) > 0 {
		disk.AppPassword = firstNonEmpty(disk.AppPasswords[0].Password, disk.AppPasswords[0].AppPassword)
		if disk.ICloudEmail == "" {
			disk.ICloudEmail = disk.AppPasswords[0].ICloudEmail
		}
	}

	return &Account{
		ID:            disk.ID,
		Name:          disk.Name,
		RealEmail:     disk.RealEmail,
		ICloudEmail:   disk.ICloudEmail,
		Cookies:       cookies,
		Host:          disk.Host,
		Proxy:         disk.Proxy,
		AppPassword:   disk.AppPassword,
		Status:        disk.Status,
		AliasTotal:    disk.AliasTotal,
		AliasActive:   disk.AliasActive,
		LastValidated: disk.LastValidated,
		LastError:     disk.LastError,
		CreatedAt:     disk.CreatedAt,
	}, nil
}

func parseCookiesJSON(raw json.RawMessage) (map[string]string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return make(map[string]string), nil
	}
	switch raw[0] {
	case '{':
		var cookies map[string]string
		if err := json.Unmarshal(raw, &cookies); err != nil {
			return nil, err
		}
		if cookies == nil {
			cookies = make(map[string]string)
		}
		return cookies, nil
	case '[':
		var exported []browserCookie
		if err := json.Unmarshal(raw, &exported); err != nil {
			return nil, err
		}
		cookies := make(map[string]string, len(exported))
		for _, c := range exported {
			if c.Name != "" {
				cookies[c.Name] = c.Value
			}
		}
		return cookies, nil
	case '"':
		var cookieInput string
		if err := json.Unmarshal(raw, &cookieInput); err != nil {
			return nil, err
		}
		return ParseCookieInput(cookieInput)
	default:
		return nil, fmt.Errorf("cookies 必须是对象、数组或字符串")
	}
}

func firstJSONByte(raw []byte) byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0
	}
	return raw[0]
}

func isEditedExampleAccount(acc *Account) bool {
	if acc == nil {
		return false
	}
	for _, value := range acc.Cookies {
		if isUserConfiguredValue(value) {
			return true
		}
	}
	return isUserConfiguredValue(acc.RealEmail) ||
		isUserConfiguredValue(acc.ICloudEmail) ||
		isUserConfiguredValue(acc.AppPassword)
}

func isUserConfiguredValue(value string) bool {
	normalized := strings.ToLower(strings.Trim(strings.TrimSpace(value), `"'`))
	if normalized == "" {
		return false
	}
	placeholders := []string{
		"paste_your_cookie_value_here",
		"your_email@icloud.com",
		"xxxx-xxxx-xxxx-xxxx",
		"value",
	}
	for _, placeholder := range placeholders {
		if normalized == placeholder {
			return false
		}
	}
	return true
}

func (m *Manager) save() error {
	wrapper := struct {
		Accounts  map[string]*Account `json:"accounts"`
		UpdatedAt string              `json:"updated_at"`
	}{
		Accounts:  m.accounts,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}
	raw, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.dataDir, ".accounts-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), m.dataFile)
}

// ParseCookieInput 解析 Cookie 输入,支持两种格式:
//   - Header String: "name1=value1; name2=value2; ..."
//   - JSON: {"name1":"value1","name2":"value2"}
//
// 空输入返回错误。
func ParseCookieInput(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("空白输入 — 请粘贴 Cookie Header String 或 JSON")
	}

	// JSON 格式
	if strings.HasPrefix(raw, "{") {
		var cookies map[string]string
		if err := json.Unmarshal([]byte(raw), &cookies); err == nil && cookies != nil {
			out := make(map[string]string, len(cookies))
			for k, v := range cookies {
				if v != "" {
					out[k] = v
				}
			}
			if len(out) > 0 {
				return out, nil
			}
		}
	}

	// Header String 格式
	cookies := make(map[string]string)
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		idx := strings.Index(part, "=")
		if idx <= 0 {
			continue
		}
		name := strings.TrimSpace(part[:idx])
		value := strings.TrimSpace(part[idx+1:])
		if name != "" {
			cookies[name] = value
		}
	}
	if len(cookies) == 0 {
		return nil, fmt.Errorf("无法解析 Cookie 输入,请提供 Header String 或 JSON 格式")
	}
	return cookies, nil
}

// AddAccount 添加一个账号。cookieInput 可为空,后续可通过 /login 获取。
//
// cookieInput 支持 Header String 或 JSON。校验失败仍会保存账号(status=error),
// 方便用户后续修正 Cookie 后重新校验。
func (m *Manager) AddAccount(name, cookieInput, host, proxy string, realEmail ...string) (*Account, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("账号名称不能为空")
	}
	var cookies map[string]string
	if cookieInput != "" {
		var err error
		cookies, err = ParseCookieInput(cookieInput)
		if err != nil {
			return nil, err
		}
	} else {
		cookies = make(map[string]string)
	}
	if host == "" {
		host = "icloud.com"
	}

	acc := &Account{
		ID:        "acc_" + uuid.New().String()[:8],
		Name:      name,
		Cookies:   cookies,
		Host:      host,
		Proxy:     proxy,
		Status:    "pending", // 无 Cookie 时为 pending
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	if len(realEmail) > 0 {
		acc.RealEmail = strings.TrimSpace(realEmail[0])
	}

	// 有 Cookie 才校验会话
	if len(cookies) > 0 {
		client, err := hme.NewClient(cookies, host, proxy, false)
		if err != nil {
			return nil, err
		}
		if err := client.ValidateSession(); err != nil {
			acc.Status = "error"
			acc.LastError = truncate(err.Error(), 300)
		} else {
			acc.Status = "active"
			if info := client.AccountInfo(); info != nil {
				acc.RealEmail = firstNonEmpty(info.AppleID, info.PrimaryEmail)
				acc.ICloudEmail = deriveICloudEmail(info)
			}
			if aliases, err := client.ListAliases(); err == nil {
				acc.AliasTotal = len(aliases)
				for _, a := range aliases {
					if a.Active {
						acc.AliasActive++
					}
				}
			}
			acc.LastValidated = time.Now().Format(time.RFC3339)
		}
		acc.Cookies = cloneCookies(client.Cookies)
	}

	m.mu.Lock()
	m.accounts[acc.ID] = acc
	saveErr := m.save()
	if saveErr != nil {
		delete(m.accounts, acc.ID)
	}
	m.mu.Unlock()
	if saveErr != nil {
		return nil, saveErr
	}
	return cloneAccount(acc), nil
}

// RemoveAccount 删除账号。
func (m *Manager) RemoveAccount(id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return false, nil
	}
	delete(m.accounts, id)
	if err := m.save(); err != nil {
		m.accounts[id] = acc
		return false, err
	}
	m.forgetAccountNetwork(id)
	m.forgetIMAP(acc)
	return true, nil
}

func (m *Manager) forgetIMAP(acc *Account) {
	if m.imapPool == nil || acc == nil {
		return
	}
	for _, email := range []string{acc.ICloudEmail, acc.RealEmail} {
		if email != "" {
			m.imapPool.Forget(email)
		}
	}
}

// GetAccount 返回账号副本。
func (m *Manager) GetAccount(id string) (*Account, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return nil, false
	}
	return cloneAccount(acc), true
}

func cloneCookies(cookies map[string]string) map[string]string {
	copy := make(map[string]string, len(cookies))
	for name, value := range cookies {
		copy[name] = value
	}
	return copy
}

func cloneAccount(acc *Account) *Account {
	copy := *acc
	copy.Cookies = cloneCookies(acc.Cookies)
	return &copy
}

// Public returns a detached account suitable for API responses.
func (acc *Account) Public() *Account {
	copy := *acc
	copy.HasCookies = acc.hasUsableCookies()
	if !copy.HasCookies && (acc.LastValidated != "" || acc.Status == "active") {
		copy.Status = statusReauthRequired
		copy.LastError = ErrHMEReauthRequired.Error()
	}
	copy.HasAppPassword = acc.AppPassword != ""
	copy.Cookies = nil
	copy.AppPassword = ""
	copy.Proxy = ""
	copy.SessionToken = ""
	copy.TrustToken = ""
	return &copy
}

func (acc *Account) hasUsableCookies() bool {
	return len(acc.Cookies) > 0 && acc.Status != statusReauthRequired
}

// Apply updates while locked, rolling back if persistence fails.
func (m *Manager) updateAccount(id string, update func(*Account)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updateAccountLocked(id, update)
}

func (m *Manager) updateAccountLocked(id string, update func(*Account)) error {
	previous, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	next := cloneAccount(previous)
	update(next)
	m.accounts[id] = next
	if err := m.save(); err != nil {
		m.accounts[id] = previous
		return err
	}
	return nil
}

// ListAccounts 返回所有账号(脱敏,不含 Cookies),按活跃状态排序。
func (m *Manager) ListAccounts() []*Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Account, 0, len(m.accounts))
	for _, acc := range m.accounts {
		out = append(out, acc.Public())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// HMEClient 为指定账号创建一个新的 HME 客户端。
// 必须有有效的 Cookie 才能使用 HME 功能。
func (m *Manager) HMEClient(id string, verbose bool) (*hme.Client, error) {
	acc, ok := m.GetAccount(id)
	if !ok {
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	if !acc.hasUsableCookies() {
		return nil, ErrHMEReauthRequired
	}
	transport, err := m.transportFor(id, acc.Proxy)
	if err != nil {
		return nil, err
	}
	client := hme.NewSessionClient(acc.Cookies, acc.Host, verbose, transport)
	host := client.Host
	if session, ok := m.cachedSession(id, host); ok {
		client.UseSession(session)
	}
	client.OnValidated = func(session hme.Session) { m.storeSession(id, host, session) }
	client.SetAccountCountry(acc.AccountCountry)
	// 会话失效时,提供登录时存下的长期 token 供免密重建。
	client.Reauth = func() (string, string, bool) {
		cur, ok := m.GetAccount(id)
		if !ok || cur.SessionToken == "" {
			return "", "", false
		}
		return cur.SessionToken, cur.TrustToken, true
	}
	return client, nil
}

// ErrLoginNeedsOTP 表示登录到了需要 2FA 的一步,验证码已推送到受信任设备,
// 应调用 CompleteLoginOTP 在同一握手上提交验证码。
var ErrLoginNeedsOTP = errors.New("请输入受信任 Apple 设备上的 6 位验证码")

// ErrNoPendingLogin 表示没有待验证的登录会话(未开始、已过期或已用完)。
var ErrNoPendingLogin = errors.New("登录会话已过期,请重新用密码登录")

// pendingLoginTTL 是待验证登录会话的最长保留时间。
const pendingLoginTTL = 5 * time.Minute

type pendingLogin struct {
	client  *hme.Client
	expires time.Time
}

func (m *Manager) accountEmail(acc *Account) (string, error) {
	email := acc.RealEmail
	if email == "" {
		email = acc.ICloudEmail
	}
	if email == "" {
		return "", fmt.Errorf("账号未设置邮箱地址")
	}
	return email, nil
}

// BeginLogin 用密码发起登录。需要 2FA 时返回 ErrLoginNeedsOTP,并把握手会话暂存,
// 等待随后的 CompleteLoginOTP 在**同一握手**上提交验证码。不需要 2FA 时直接完成。
func (m *Manager) BeginLogin(id, password string) error {
	acc, ok := m.GetAccount(id)
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	email, err := m.accountEmail(acc)
	if err != nil {
		return err
	}
	client, err := hme.NewClient(nil, acc.Host, acc.Proxy, false)
	if err != nil {
		return err
	}
	needsOTP, err := client.LoginBegin(email, password)
	if err != nil {
		return err
	}
	if needsOTP {
		m.stashPendingLogin(id, client)
		return ErrLoginNeedsOTP
	}
	return m.saveLoggedInClient(id, client)
}

// CompleteLoginOTP 在 BeginLogin 暂存的握手上提交 2FA 验证码并完成登录。
func (m *Manager) CompleteLoginOTP(id, code string) error {
	client, ok := m.takePendingLogin(id)
	if !ok {
		return ErrNoPendingLogin
	}
	if err := client.CompleteOTP(code); err != nil {
		if errors.Is(err, hme.ErrInvalidAppleCode) && client.NeedsOTP() {
			// 验证码输错时握手仍有效,放回去让用户重填,而不是重新推送新验证码。
			m.stashPendingLogin(id, client)
		}
		return err
	}
	return m.saveLoggedInClient(id, client)
}

// RequestLoginSMS 在 BeginLogin 暂存的握手上请求短信验证码,返回脱敏的手机号。
// 握手保留(并重新计时),随后用 CompleteLoginOTP 提交短信里的验证码。
func (m *Manager) RequestLoginSMS(id string) (string, error) {
	client, ok := m.takePendingLogin(id)
	if !ok {
		return "", ErrNoPendingLogin
	}
	phone, err := client.RequestSMSCode()
	if client.NeedsOTP() {
		m.stashPendingLogin(id, client)
	}
	return phone, err
}

func (m *Manager) saveLoggedInClient(id string, client *hme.Client) error {
	if len(client.Cookies) == 0 {
		return ErrHMEReauthRequired
	}
	if err := m.updateAccount(id, func(current *Account) {
		current.Cookies = cloneCookies(client.Cookies)
		current.Status = "active"
		current.LastError = ""
		current.LastValidated = time.Now().Format(time.RFC3339)
		if t := client.SessionToken(); t != "" {
			current.SessionToken = t
			current.TrustToken = client.TrustToken()
			current.AccountCountry = client.AccountCountry()
		}
	}); err != nil {
		return err
	}
	m.invalidateSession(id)
	return nil
}

func (m *Manager) stashPendingLogin(id string, client *hme.Client) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if m.pendingLogins == nil {
		m.pendingLogins = make(map[string]pendingLogin)
	}
	m.pendingLogins[id] = pendingLogin{client: client, expires: time.Now().Add(pendingLoginTTL)}
}

// takePendingLogin 取出并移除未过期的待验证登录会话(一次性)。
func (m *Manager) takePendingLogin(id string) (*hme.Client, bool) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	entry, ok := m.pendingLogins[id]
	if !ok {
		return nil, false
	}
	delete(m.pendingLogins, id)
	if time.Now().After(entry.expires) {
		return nil, false
	}
	return entry.client, true
}

// MailClient 为指定账号创建 IMAP 邮件客户端。
// 需要事先设置 iCloud 邮箱和 App 专用密码。
func (m *Manager) MailClient(id string) (*mail.Client, error) {
	acc, ok := m.GetAccount(id)
	if !ok {
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	imapEmail := acc.ICloudEmail
	if imapEmail == "" {
		imapEmail = acc.RealEmail
	}
	if !isICloudDomain(imapEmail) {
		return nil, fmt.Errorf("账号未设置 iCloud 邮箱 (当前: %s)", imapEmail)
	}
	if acc.AppPassword == "" {
		return nil, fmt.Errorf("账号未设置 App 专用密码")
	}
	return mail.NewPooledClient(imapEmail, acc.AppPassword, m.imapPool), nil
}

// WebMailClient 为指定账号创建 Web 邮件客户端。
// 使用 Cookie 认证，无需 App Password。
func (m *Manager) WebMailClient(id string) (*mail.WebClient, error) {
	acc, ok := m.GetAccount(id)
	if !ok {
		return nil, fmt.Errorf("账号不存在: %s", id)
	}
	if !acc.hasUsableCookies() {
		return nil, fmt.Errorf("账号未配置 Cookie，无法读取邮件")
	}
	// 从 cookies 中获取 dsid
	dsid := ""
	if v, ok := acc.Cookies["X-APPLE-WEBAUTH-USER"]; ok {
		// 解析 "v=1:s=1:d=22789132008" 格式
		parts := strings.Split(v, ":d=")
		if len(parts) == 2 {
			dsid = parts[1]
		}
	}
	return mail.NewWebClient(acc.Cookies, dsid, acc.Host), nil
}

// SetAppPassword 设置 iCloud 邮箱和 App 专用密码,并测试 IMAP 连接。
func (m *Manager) SetAppPassword(id, icloudEmail, appPassword string) error {
	previous, ok := m.GetAccount(id)
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	if icloudEmail == "" {
		return fmt.Errorf("iCloud 邮箱不能为空")
	}
	if appPassword == "" {
		return fmt.Errorf("App 专用密码不能为空")
	}

	// 测试连接
	mc := mail.NewClient(icloudEmail, appPassword)
	if err := mc.Connect(); err != nil {
		return err
	}
	count, err := mc.InboxCount()
	mc.Disconnect()
	if err != nil {
		return err
	}

	err = m.updateAccount(id, func(acc *Account) {
		acc.ICloudEmail = strings.TrimSpace(icloudEmail)
		acc.AppPassword = appPassword
	})
	if err != nil {
		return err
	}
	m.forgetIMAP(previous)
	_ = count
	return nil
}

// SaveCookies 保存指定账号的最新 Cookie（HMEClient 操作后刷新的 token）。
// 用于客户端 validate/操作过程中从 Set-Cookie 获取了新 token 后持久化。
func (m *Manager) SaveCookies(id string, cookies map[string]string) error {
	return m.updateAccount(id, func(acc *Account) {
		acc.Cookies = cloneCookies(cookies)
	})
}

// Late network responses must not replace credentials saved by a newer request.
func (m *Manager) SaveCookiesIfUnchanged(id string, before, after map[string]string) error {
	return m.saveCookieResponse(id, before, after, false)
}

func (m *Manager) SaveHMEState(id string, before, after map[string]string, requestErr error) error {
	return m.saveCookieResponse(id, before, after, hme.IsAuthenticationError(requestErr))
}

func (m *Manager) saveCookieResponse(id string, before, after map[string]string, rejected bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, ok := m.accounts[id]
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}
	if !maps.Equal(acc.Cookies, before) || acc.Status == statusReauthRequired || (!rejected && maps.Equal(before, after)) {
		return nil
	}
	if rejected || len(after) == 0 {
		m.invalidateSession(id)
	}
	return m.updateAccountLocked(id, func(acc *Account) {
		if rejected || len(after) == 0 {
			acc.Status = statusReauthRequired
			acc.LastError = ErrHMEReauthRequired.Error()
			return
		}
		acc.Cookies = cloneCookies(after)
	})
}

// UpdateCookies 更新指定账号的 Cookie,并自动校验会话有效性。
func (m *Manager) UpdateCookies(id string, cookies map[string]string) error {
	if len(cookies) == 0 {
		return fmt.Errorf("cookies 不能为空")
	}
	acc, ok := m.GetAccount(id)
	if !ok {
		return fmt.Errorf("账号不存在: %s", id)
	}

	if acc.Host == "" {
		acc.Host = "icloud.com"
	}
	client, err := hme.NewClient(cookies, acc.Host, acc.Proxy, false)
	if err != nil {
		return err
	}
	if err := client.ValidateSession(); err != nil {
		return fmt.Errorf("新 Cookie 校验失败，请从已登录的 iCloud 网页重新获取")
	}
	if len(client.Cookies) == 0 {
		return ErrHMEReauthRequired
	}

	if err := m.updateAccount(id, func(current *Account) {
		current.Cookies = cloneCookies(client.Cookies)
		current.Host = acc.Host
		current.Status = "active"
		current.LastError = ""
		current.LastValidated = time.Now().Format(time.RFC3339)
		if info := client.AccountInfo(); info != nil {
			current.RealEmail = firstNonEmpty(info.AppleID, info.PrimaryEmail, current.RealEmail)
			if current.ICloudEmail == "" {
				current.ICloudEmail = deriveICloudEmail(info)
			}
		}
	}); err != nil {
		return err
	}
	// 刚校验过新 Cookie,直接缓存这次的会话,下次请求无需再 /validate。
	m.storeSession(id, client.Host, client.Session())
	return nil
}

// ---- 辅助函数 ----

// deriveICloudEmail 从账号身份推导 iCloud 邮箱地址(用于 IMAP 登录)。
//
// 规则:
//  1. primaryEmail 是 @icloud.com/@me.com/@mac.com → 直接用
//  2. appleId 是上述域名 → 直接用
//  3. appleId 是第三方邮箱(如 @qq.com) → 取 local part 拼 @icloud.com
func deriveICloudEmail(info *hme.AccountInfo) string {
	primary := strings.TrimSpace(info.PrimaryEmail)
	appleID := strings.TrimSpace(info.AppleID)

	if isICloudDomain(primary) {
		return primary
	}
	if isICloudDomain(appleID) {
		return appleID
	}
	if strings.Contains(appleID, "@") {
		local := strings.SplitN(appleID, "@", 2)[0]
		return local + "@icloud.com"
	}
	return firstNonEmpty(primary, appleID)
}

func isICloudDomain(email string) bool {
	return email != "" && (strings.Contains(email, "@icloud.com") ||
		strings.Contains(email, "@me.com") ||
		strings.Contains(email, "@mac.com"))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

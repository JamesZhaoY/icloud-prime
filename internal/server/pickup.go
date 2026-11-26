package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/createjob"
	"icloud-hme/internal/hme"
	icloudmail "icloud-hme/internal/mail"
)

func (s *Server) SetPublicURL(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		s.publicURL = ""
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return fmt.Errorf("public URL must be an HTTP(S) origin")
	}
	s.publicURL = parsed.Scheme + "://" + parsed.Host
	return nil
}

func makePickupTarget(accountID, alias string) pickupTarget {
	return pickupTarget{AccountID: accountID, Alias: strings.ToLower(strings.TrimSpace(alias))}
}

func (s *Server) pickupURL(secret string) string {
	if secret == "" {
		return ""
	}
	return s.publicURL + "/api/pickup/" + secret
}

func (s *Server) issuePickupURLs(targets []pickupTarget, rotate bool) (map[pickupTarget]string, error) {
	if s.pickups == nil {
		return nil, fmt.Errorf("pickup link store not configured")
	}
	return s.pickups.issue(targets, rotate)
}

func (s *Server) pickupURLs(targets []pickupTarget) map[pickupTarget]string {
	secrets, err := s.issuePickupURLs(targets, false)
	if err != nil && s.pickups != nil {
		log.Printf("无法保存取件地址: %v", err)
	}
	return secrets
}

func (s *Server) issuePickupURL(accountID, alias string) string {
	target := makePickupTarget(accountID, alias)
	return s.pickupURL(s.pickupURLs([]pickupTarget{target})[target])
}

func (s *Server) validatePickup(c *gin.Context, accountID, alias string) (string, bool) {
	if accountID == "" {
		fail(c, http.StatusBadRequest, "参数缺失: account_id")
		return "", false
	}
	alias = strings.ToLower(strings.TrimSpace(alias))
	address, err := mail.ParseAddress(alias)
	if err != nil || address.Address != alias || len(alias) > 254 || strings.ContainsAny(alias, "/\\") {
		fail(c, http.StatusBadRequest, "无效的邮箱别名")
		return "", false
	}
	if _, exists := s.mgr.GetAccount(accountID); !exists {
		fail(c, http.StatusNotFound, "账号不存在")
		return "", false
	}
	return alias, true
}

func (s *Server) getPickupAddress(c *gin.Context) {
	s.managePickupAddress(c, c.Query("account_id"), c.Query("alias"), false)
}

func (s *Server) rotatePickupAddress(c *gin.Context) {
	var request struct {
		AccountID string `json:"account_id"`
		Alias     string `json:"alias"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		fail(c, http.StatusBadRequest, "参数错误: 需要 account_id 和 alias")
		return
	}
	s.managePickupAddress(c, request.AccountID, request.Alias, true)
}

func (s *Server) managePickupAddress(c *gin.Context, accountID, rawAlias string, rotate bool) {
	c.Header("Cache-Control", "no-store")
	alias, valid := s.validatePickup(c, accountID, rawAlias)
	if !valid {
		return
	}
	target := makePickupTarget(accountID, alias)
	aliases, _, err := s.findAliases(accountID, false)
	if err != nil {
		fail(c, http.StatusBadGateway, "无法验证 iCloud 别名，请检查登录状态")
		return
	}
	if !activePickupAlias(aliases, target) {
		fail(c, http.StatusNotFound, "未找到启用中的邮箱别名")
		return
	}
	secrets, err := s.issuePickupURLs([]pickupTarget{target}, rotate)
	if err != nil {
		fail(c, http.StatusInternalServerError, "无法保存取件地址")
		return
	}
	ok(c, gin.H{"account_id": accountID, "email": alias, "pickup_url": s.pickupURL(secrets[target])})
}

func (s *Server) pickupInbox(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	accountID := c.Param("secret")
	if alias, valid := s.validatePickup(c, accountID, c.Param("alias")); valid {
		s.inbox(c, accountID, alias)
	}
}

var sharedPickupSlots = make(chan struct{}, 8)

func (s *Server) sharedPickupInbox(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Writer.Header().Add("Vary", "Accept")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Robots-Tag", "noindex, nofollow")
	page := pickupPageData{
		Limit: pickupBound(c.Query("limit"), 1, 30),
		Days:  pickupBound(c.Query("days"), 7, 30),
	}
	showPage := c.Query("format") == "html" || (c.Query("format") == "" && c.NegotiateFormat("application/json", "text/html") == "text/html")
	respondError := func(status int, message string) {
		if showPage {
			page.Error = message
			renderPickupPage(c, status, page)
			return
		}
		fail(c, status, message)
	}
	secret := c.Param("secret")
	if s.pickups == nil || !validPickupSecret(secret) {
		respondError(http.StatusNotFound, "取件地址无效")
		return
	}
	target, exists := s.pickups.lookup(secret)
	if !exists {
		respondError(http.StatusNotFound, "取件地址无效")
		return
	}
	if _, exists := s.mgr.GetAccount(target.AccountID); !exists {
		respondError(http.StatusNotFound, "取件地址无效")
		return
	}
	select {
	case sharedPickupSlots <- struct{}{}:
		defer func() { <-sharedPickupSlots }()
	default:
		respondError(http.StatusTooManyRequests, "取件繁忙，请稍后重试")
		return
	}
	aliases, cached := s.cachedAliases(target.AccountID)
	if !cached {
		// 缓存过期但不太旧:先用旧列表校验,后台刷新,避免每次取件都等 iCloud 别名列表。
		if stale, ok := s.staleAliases(target.AccountID, s.pickupStaleTTL); ok {
			aliases, cached = stale, true
			s.refreshAliasesInBackground(target.AccountID)
		}
	}
	var verification <-chan int
	if cached {
		if !activePickupAlias(aliases, target) {
			respondError(http.StatusNotFound, "取件地址无效")
			return
		}
	} else {
		result := make(chan int, 1)
		verification = result
		go func() {
			aliases, _, err := s.findAliases(target.AccountID, false)
			switch {
			case err != nil:
				result <- http.StatusBadGateway
			case !activePickupAlias(aliases, target):
				result <- http.StatusNotFound
			default:
				result <- http.StatusOK
			}
		}()
	}
	withBody := showPage || c.Query("body") == "1" || c.Query("body") == "true"
	messages, method, err := s.sharedMessages(target, page.Limit, page.Days, withBody)
	if verification != nil {
		switch <-verification {
		case http.StatusBadGateway:
			respondError(http.StatusBadGateway, "暂时无法验证取件地址")
			return
		case http.StatusNotFound:
			respondError(http.StatusNotFound, "取件地址无效")
			return
		}
	}
	if err != nil {
		if errors.Is(err, icloudmail.ErrPickupNeedsIMAP) {
			respondError(http.StatusServiceUnavailable, "请让邮箱管理员配置 IMAP；网页收信无法核实每封邮件的收件地址")
			return
		}
		respondError(http.StatusBadGateway, "暂时无法安全读取邮件，请检查 iCloud 收信配置")
		return
	}
	if _, exists := s.pickups.lookup(secret); !exists {
		respondError(http.StatusNotFound, "取件地址无效")
		return
	}
	if showPage {
		page.Alias = target.Alias
		page.Messages = messages
		renderPickupPage(c, http.StatusOK, page)
		return
	}
	ok(c, gin.H{"alias": target.Alias, "count": len(messages), "messages": messages, "method": method})
}

func pickupBound(raw string, fallback, maximum int) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return fallback
	}
	return min(value, maximum)
}

func activePickupAlias(aliases []hme.Alias, target pickupTarget) bool {
	for _, alias := range aliases {
		if alias.Active && strings.EqualFold(alias.Email, target.Alias) {
			return true
		}
	}
	return false
}

func (s *Server) sharedMessages(target pickupTarget, limit, days int, body bool) ([]icloudmail.Message, string, error) {
	if client, err := s.mgr.MailClient(target.AccountID); err == nil {
		if err := client.Connect(); err == nil {
			defer client.Disconnect()
			messages, err := client.PickupByRecipient(target.Alias, limit, days, body)
			if err == nil {
				return messages, "imap", nil
			}
		}
	}
	client, err := s.mgr.WebMailClient(target.AccountID)
	if err != nil {
		return nil, "", err
	}
	before := client.CookiesSnapshot()
	defer func() { _ = s.mgr.SaveCookiesIfUnchanged(target.AccountID, before, client.CookiesSnapshot()) }()
	messages, err := client.PickupByRecipient(target.Alias, limit)
	return messages, "web_api", err
}

type aliasWithPickup struct {
	hme.Alias
	PickupURL string `json:"pickup_url,omitempty"`
}

func (s *Server) aliasesWithPickup(accountID string, aliases []hme.Alias) []aliasWithPickup {
	result := make([]aliasWithPickup, 0, len(aliases))
	var targets []pickupTarget
	for _, alias := range aliases {
		if alias.Active {
			targets = append(targets, makePickupTarget(accountID, alias.Email))
		}
	}
	secrets := s.pickupURLs(targets)
	for _, alias := range aliases {
		result = append(result, aliasWithPickup{Alias: alias, PickupURL: s.pickupURL(secrets[makePickupTarget(accountID, alias.Email)])})
	}
	return result
}

type createdWithPickup struct {
	createjob.CreateResult
	PickupURL string `json:"pickup_url,omitempty"`
}

func (s *Server) batchWithPickup(batch *createjob.BatchResponse) any {
	if batch == nil {
		return nil
	}
	created := make([]createdWithPickup, 0, len(batch.Created))
	targets := make([]pickupTarget, 0, len(batch.Created))
	for _, item := range batch.Created {
		targets = append(targets, makePickupTarget(batch.AccountID, item.Email))
	}
	secrets := s.pickupURLs(targets)
	for _, item := range batch.Created {
		created = append(created, createdWithPickup{CreateResult: item, PickupURL: s.pickupURL(secrets[makePickupTarget(batch.AccountID, item.Email)])})
	}
	return struct {
		*createjob.BatchResponse
		Created []createdWithPickup `json:"created"`
	}{batch, created}
}

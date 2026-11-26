package server

import (
	"bytes"
	_ "embed"
	"html/template"
	"net/http"

	"github.com/gin-gonic/gin"
	icloudmail "icloud-hme/internal/mail"
)

//go:embed pickup_page.html
var pickupPageHTML string

var pickupPageTemplate = template.Must(template.New("pickup").Funcs(template.FuncMap{
	"emailOff": func() template.HTML { return "<!--email_off-->" },
	"emailOn":  func() template.HTML { return "<!--/email_off-->" },
	"mailTime": icloudmail.DisplayMessageTime,
}).Parse(pickupPageHTML))

type pickupPageData struct {
	Alias    string
	Limit    int
	Days     int
	Messages []icloudmail.Message
	Error    string
}

func renderPickupPage(c *gin.Context, status int, page pickupPageData) {
	var output bytes.Buffer
	if err := pickupPageTemplate.Execute(&output, page); err != nil {
		c.String(http.StatusInternalServerError, "无法显示邮件页面")
		return
	}
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	c.Data(status, "text/html; charset=utf-8", output.Bytes())
}

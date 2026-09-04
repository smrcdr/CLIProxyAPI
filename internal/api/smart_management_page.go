package api

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed assets/smart-management.html
var smartManagementHTML []byte

func (s *Server) serveSmartManagementPage(c *gin.Context) {
	if s == nil || s.cfg == nil || !s.cfg.SmartManagementEnabled {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/html; charset=utf-8", smartManagementHTML)
}

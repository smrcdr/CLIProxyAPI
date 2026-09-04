package api

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

//go:embed assets/router-management.html
var routerManagementHTML []byte

func (s *Server) serveRouterManagementPage(c *gin.Context) {
	if s == nil || s.cfg == nil || s.cfg.ServiceRole != config.ServiceRoleRouter || s.routerManagement == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/html; charset=utf-8", routerManagementHTML)
}

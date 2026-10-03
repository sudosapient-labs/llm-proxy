package management

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotaobserver"
)

var quotaSimulationAliasPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,31}$`)

func (h *Handler) SetQuotaForecastSource(source func(string, string) (quotaobserver.Status, error)) {
	h.mu.Lock()
	h.quotaForecastSource = source
	h.mu.Unlock()
}

func (h *Handler) GetQuotaForecast(c *gin.Context) {
	h.quotaForecastResponse(c, "", "")
}

// SimulateQuotaForecast reads cached quota and affinity; it cannot send upstream
// requests, activate credentials, select an auth, or mutate actual bindings.
func (h *Handler) SimulateQuotaForecast(c *gin.Context) {
	var request *struct {
		SessionID string `json:"session_id"`
		Pinned    string `json:"pinned"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8192)
	// Read the complete bounded body so trailing JSON or whitespace cannot
	// bypass validation after the first object has been decoded.
	data, errRead := io.ReadAll(c.Request.Body)
	if errRead != nil || json.Unmarshal(data, &request) != nil || request == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quota simulation request"})
		return
	}
	pinned := strings.TrimSpace(request.Pinned)
	if len(request.SessionID) > 1024 || len(request.Pinned) > 32 || (pinned != "" && !quotaSimulationAliasPattern.MatchString(pinned)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quota simulation request"})
		return
	}
	h.quotaForecastResponse(c, strings.TrimSpace(request.SessionID), pinned)
}

func (h *Handler) quotaForecastResponse(c *gin.Context, sessionID, pinned string) {
	h.mu.Lock()
	source := h.quotaForecastSource
	h.mu.Unlock()
	if source == nil {
		c.JSON(http.StatusOK, quotaobserver.Status{Enabled: false, Mode: "observe-and-simulate"})
		return
	}
	status, errSnapshot := source(sessionID, pinned)
	if errSnapshot != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quota forecast unavailable"})
		return
	}
	c.JSON(http.StatusOK, status)
}

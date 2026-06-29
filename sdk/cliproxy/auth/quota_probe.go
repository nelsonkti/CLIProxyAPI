package auth

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	codexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"

	// codexFiveHourWindowSeconds identifies the Codex primary 5-hour window.
	codexFiveHourWindowSeconds = 18000

	// quotaThresholdCooldownMaxCap bounds the proactive cooldown duration as a
	// guard against dirty upstream reset timestamps.
	quotaThresholdCooldownMaxCap = 6 * time.Hour
)

// quotaProbeResult is the normalized usage signal for the 5-hour window.
type quotaProbeResult struct {
	usedPercent float64
	resetAt     time.Time
}

// quotaProbe inspects an account's 5-hour usage window from a dedicated
// upstream usage endpoint. Implementations are provider-specific.
type quotaProbe interface {
	probe(ctx context.Context, auth *Auth) (quotaProbeResult, bool)
}

// quotaProbeForProvider returns the probe for a provider, or nil when the
// provider has no proactive-cooldown support.
func quotaProbeForProvider(provider string) quotaProbe {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return claudeQuotaProbe{}
	case "codex":
		return codexQuotaProbe{}
	default:
		return nil
	}
}

// runQuotaThresholdProbe checks a single account's 5-hour utilization against
// its per-account threshold and, when exceeded, proactively cools the whole
// account until the window reset. It is a no-op for accounts without a
// threshold, already-cooled accounts, or unsupported providers.
func (m *Manager) runQuotaThresholdProbe(ctx context.Context, authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.RLock()
	auth := m.auths[authID]
	var cloned *Auth
	if auth != nil {
		cloned = auth.Clone()
	}
	m.mu.RUnlock()
	if cloned == nil {
		return
	}

	threshold, ok := quotaCooldownThreshold(cloned)
	if !ok {
		return
	}
	// Skip accounts already cooling down.
	now := time.Now()
	if cloned.Unavailable && cloned.NextRetryAfter.After(now) {
		return
	}

	probe := quotaProbeForProvider(cloned.Provider)
	if probe == nil {
		return
	}

	result, okProbe := probe.probe(ctx, cloned)
	if !okProbe {
		return
	}
	if result.usedPercent < float64(threshold) {
		return
	}
	recoverAt := result.resetAt
	if recoverAt.IsZero() || !recoverAt.After(now) {
		return
	}
	if recoverAt.Sub(now) > quotaThresholdCooldownMaxCap {
		recoverAt = now.Add(quotaThresholdCooldownMaxCap)
	}

	log.Debugf("quota threshold cooldown: provider=%s auth=%s used=%.1f%% threshold=%d%% until=%s",
		cloned.Provider, cloned.ID, result.usedPercent, threshold, recoverAt.Format(time.RFC3339))
	m.ApplyQuotaThresholdCooldown(ctx, authID, recoverAt)
}

// quotaCooldownThreshold reads the per-account threshold from attributes.
// A value <= 0 (or absent/invalid) means "no limit" and returns false.
func quotaCooldownThreshold(auth *Auth) (int, bool) {
	if auth == nil || auth.Attributes == nil {
		return 0, false
	}
	raw := strings.TrimSpace(auth.Attributes["quota_cooldown_threshold"])
	if raw == "" {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, false
	}
	if value > 100 {
		value = 100
	}
	return value, true
}

// quotaProbeHTTPClient builds an HTTP client honoring the per-auth proxy.
// No response timeout is set after the connection is established.
func quotaProbeHTTPClient(auth *Auth) *http.Client {
	transport := &http.Transport{}
	if auth != nil {
		if proxyStr := strings.TrimSpace(auth.ProxyURL); proxyStr != "" {
			if proxyURL, err := url.Parse(proxyStr); err == nil {
				transport.Proxy = http.ProxyURL(proxyURL)
			}
		}
	}
	return &http.Client{Transport: transport}
}

// accessTokenFromMetadata extracts an OAuth access token from auth metadata.
func accessTokenFromMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	if v, ok := metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["accessToken"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if tokenRaw, ok := metadata["token"]; ok {
		switch typed := tokenRaw.(type) {
		case string:
			if v := strings.TrimSpace(typed); v != "" {
				return v
			}
		case map[string]any:
			if v, ok := typed["access_token"].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// readUsageBody performs a GET against rawURL with the provided headers and
// returns the response body on HTTP 200.
func readUsageBody(ctx context.Context, auth *Auth, rawURL string, headers map[string]string) ([]byte, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := quotaProbeHTTPClient(auth).Do(req)
	if err != nil {
		return nil, false
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("quota probe: close body error: %v", errClose)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	return body, true
}

// claudeQuotaProbe reads the Claude five_hour window utilization.
type claudeQuotaProbe struct{}

func (claudeQuotaProbe) probe(ctx context.Context, auth *Auth) (quotaProbeResult, bool) {
	token := accessTokenFromMetadata(auth.Metadata)
	if token == "" {
		return quotaProbeResult{}, false
	}
	headers := map[string]string{
		"Authorization":  "Bearer " + token,
		"Content-Type":   "application/json",
		"anthropic-beta": "oauth-2025-04-20",
	}
	body, ok := readUsageBody(ctx, auth, claudeUsageURL, headers)
	if !ok {
		return quotaProbeResult{}, false
	}
	window := gjson.GetBytes(body, "five_hour")
	if !window.Exists() {
		return quotaProbeResult{}, false
	}
	used := window.Get("utilization")
	if !used.Exists() {
		return quotaProbeResult{}, false
	}
	resetAt := parseResetTimestamp(window.Get("resets_at"))
	return quotaProbeResult{usedPercent: used.Float(), resetAt: resetAt}, true
}

// codexQuotaProbe reads the Codex primary 5-hour window used_percent.
type codexQuotaProbe struct{}

func (codexQuotaProbe) probe(ctx context.Context, auth *Auth) (quotaProbeResult, bool) {
	token := accessTokenFromMetadata(auth.Metadata)
	if token == "" {
		return quotaProbeResult{}, false
	}
	headers := map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
	}
	if accountID := codexAccountID(auth); accountID != "" {
		headers["Chatgpt-Account-Id"] = accountID
	}
	body, ok := readUsageBody(ctx, auth, codexUsageURL, headers)
	if !ok {
		return quotaProbeResult{}, false
	}
	// Locate the window whose limit_window_seconds marks the 5-hour tier.
	for _, path := range []string{"rate_limit.primary_window", "rate_limit.secondary_window"} {
		window := gjson.GetBytes(body, path)
		if !window.Exists() {
			continue
		}
		if window.Get("limit_window_seconds").Int() != codexFiveHourWindowSeconds {
			continue
		}
		used := window.Get("used_percent")
		if !used.Exists() {
			continue
		}
		resetAt := codexResetTimestamp(window)
		return quotaProbeResult{usedPercent: used.Float(), resetAt: resetAt}, true
	}
	return quotaProbeResult{}, false
}

// codexAccountID resolves the ChatGPT account id from metadata or the id_token.
func codexAccountID(auth *Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if v, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if idToken, ok := auth.Metadata["id_token"].(string); ok && strings.TrimSpace(idToken) != "" {
		if claims, err := codex.ParseJWTToken(idToken); err == nil && claims != nil {
			if id := strings.TrimSpace(claims.GetAccountID()); id != "" {
				return id
			}
		}
	}
	return ""
}

// parseResetTimestamp parses an ISO-8601 timestamp from a gjson result.
func parseResetTimestamp(value gjson.Result) time.Time {
	if !value.Exists() {
		return time.Time{}
	}
	raw := strings.TrimSpace(value.String())
	if raw == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts
	}
	return time.Time{}
}

// codexResetTimestamp resolves the window reset time from reset_at (unix
// seconds) or reset_after_seconds (relative).
func codexResetTimestamp(window gjson.Result) time.Time {
	if resetAt := window.Get("reset_at").Int(); resetAt > 0 {
		return time.Unix(resetAt, 0)
	}
	if resetAfter := window.Get("reset_after_seconds").Int(); resetAfter > 0 {
		return time.Now().Add(time.Duration(resetAfter) * time.Second)
	}
	return time.Time{}
}

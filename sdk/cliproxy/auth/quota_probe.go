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

// usageBodyParserForURL returns the body parser for a known usage endpoint, or
// nil when rawURL is not a recognized usage URL. The parser is reused by the
// proactive probe and the reactive management api-call path so the body is
// interpreted identically. Comparison is on the normalized
// scheme://host/path (host lowercased, trailing slash trimmed, query and
// fragment ignored) so that callers may pass the request URL verbatim.
func usageBodyParserForURL(rawURL string) func([]byte) (quotaProbeResult, bool) {
	normalized, ok := normalizeUsageURL(rawURL)
	if !ok {
		return nil
	}
	switch normalized {
	case claudeUsageURL:
		return parseClaudeUsageBody
	case codexUsageURL:
		return parseCodexUsageBody
	default:
		return nil
	}
}

// normalizeUsageURL parses rawURL and returns a normalized
// scheme://host/path string (host lowercased, path trailing-slash trimmed) for
// comparison against the known usage endpoint constants. Returns false when
// rawURL cannot be parsed or is missing scheme/host.
func normalizeUsageURL(rawURL string) (string, bool) {
	if strings.TrimSpace(rawURL) == "" {
		return "", false
	}
	parsed, errParse := url.Parse(rawURL)
	if errParse != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}
	host := strings.ToLower(parsed.Host)
	path := strings.Trim(parsed.Path, "/")
	if path == "" {
		return parsed.Scheme + "://" + host, true
	}
	return parsed.Scheme + "://" + host + "/" + path, true
}

// ApplyQuotaCooldownFromUsageResponse evaluates a fetched usage response body
// and applies the per-account quota threshold cooldown when the remaining
// quota is below the threshold. It lets the management api-call endpoint apply
// cooldown immediately on the UI's manual usage fetch, instead of waiting for
// the next proactive probe tick. It is a no-op when requestURL is not a
// recognized usage endpoint, the account has no threshold, cooldown is
// disabled, is already cooling, or the remaining quota is above the threshold.
// body may be nil/empty.
func (m *Manager) ApplyQuotaCooldownFromUsageResponse(ctx context.Context, authID, requestURL string, body []byte) {
	if m == nil || authID == "" {
		return
	}
	parser := usageBodyParserForURL(requestURL)
	if parser == nil {
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
	// Only record a UI fetch timestamp when the account actually opts into
	// per-account threshold cooldown; otherwise the proactive probe never
	// inspects this account and the timestamp would be dead data that could
	// suppress a future probe right after a threshold is configured.
	if _, hasThreshold := quotaCooldownThreshold(cloned); hasThreshold {
		m.mu.Lock()
		m.lastUIUsageFetchAt[authID] = time.Now()
		m.mu.Unlock()
	}
	if len(body) == 0 {
		return
	}
	result, okProbe := parser(body)
	m.evaluateAndApplyQuotaCooldown(ctx, cloned, result, okProbe)
}

// runQuotaThresholdProbe checks a single account's 5-hour remaining quota
// against its per-account threshold and, when the remaining quota drops below
// the threshold, proactively cools the whole account until the window reset.
// The threshold is expressed as the remaining-quota percentage shown in the UI
// (e.g. threshold 60 cools down once less than 60% quota remains). It is a no-op
// for accounts without a threshold, already-cooled accounts, or unsupported
// providers.
func (m *Manager) runQuotaThresholdProbe(ctx context.Context, authID string) {
	if m == nil || authID == "" {
		return
	}
	// Defer to the UI-fetched path: if the management api-call endpoint
	// observed a usage fetch for this auth within the current probe
	// interval, the proactive probe would only duplicate that upstream
	// request, so skip it. The safety net remains for accounts the UI
	// never refreshes.
	m.mu.RLock()
	last := m.lastUIUsageFetchAt[authID]
	m.mu.RUnlock()
	if !last.IsZero() && time.Since(last) < refreshCheckInterval {
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

	// Skip accounts that did not opt into per-account threshold cooldown or
	// have cooldown disabled entirely. This guard must run before the probe
	// fires an upstream usage request so non-opted accounts are never
	// contacted.
	if _, hasThreshold := quotaCooldownThreshold(cloned); !hasThreshold || m.cooldownDisabledForAuth(cloned) {
		return
	}

	// Skip accounts already cooling down to avoid firing an unnecessary
	// upstream usage request.
	now := time.Now()
	if cloned.Unavailable && cloned.NextRetryAfter.After(now) {
		return
	}

	probe := quotaProbeForProvider(cloned.Provider)
	if probe == nil {
		return
	}

	result, okProbe := probe.probe(ctx, cloned)
	m.evaluateAndApplyQuotaCooldown(ctx, cloned, result, okProbe)
}

// evaluateAndApplyQuotaCooldown is the single source of truth for the
// threshold->cooldown policy. It reads the per-account threshold, skips
// accounts already cooling down, computes the remaining-quota percentage,
// validates and caps the upstream reset time, and applies the cooldown via
// ApplyQuotaThresholdCooldown. It is shared by the proactive probe
// (runQuotaThresholdProbe) and the reactive management api-call path
// (ApplyQuotaCooldownFromUsageResponse) so the two paths cannot drift.
func (m *Manager) evaluateAndApplyQuotaCooldown(ctx context.Context, auth *Auth, result quotaProbeResult, okProbe bool) {
	if m == nil || auth == nil {
		return
	}
	threshold, ok := quotaCooldownThreshold(auth)
	if !ok || m.cooldownDisabledForAuth(auth) {
		return
	}
	// Skip accounts already cooling down.
	now := time.Now()
	if auth.Unavailable && auth.NextRetryAfter.After(now) {
		return
	}
	if !okProbe {
		log.Debugf("quota threshold probe: provider=%s auth=%s usage fetch failed; skipping cooldown evaluation",
			auth.Provider, auth.ID)
		return
	}
	// Threshold matches the UI's remaining-quota percentage: cool down once the
	// remaining quota falls below the configured value.
	remainingPercent := 100 - result.usedPercent
	if remainingPercent >= float64(threshold) {
		log.Debugf("quota threshold probe: provider=%s auth=%s remaining=%.1f%% >= threshold=%d%%; no cooldown",
			auth.Provider, auth.ID, remainingPercent, threshold)
		return
	}
	recoverAt := result.resetAt
	if recoverAt.IsZero() || !recoverAt.After(now) {
		// Remaining quota is below the threshold but the upstream reset time is
		// missing or not in the future, so the cooldown window cannot be bounded.
		log.Warnf("quota threshold probe: provider=%s auth=%s remaining=%.1f%% below threshold=%d%% but reset time is invalid (resetAt=%s); cooldown skipped",
			auth.Provider, auth.ID, remainingPercent, threshold, result.resetAt.Format(time.RFC3339))
		return
	}
	if recoverAt.Sub(now) > quotaThresholdCooldownMaxCap {
		recoverAt = now.Add(quotaThresholdCooldownMaxCap)
	}

	log.Infof("quota threshold cooldown: provider=%s auth=%s remaining=%.1f%% used=%.1f%% threshold=%d%% until=%s",
		auth.Provider, auth.ID, remainingPercent, result.usedPercent, threshold, recoverAt.Format(time.RFC3339))
	m.ApplyQuotaThresholdCooldown(ctx, auth.ID, recoverAt)
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
	return parseClaudeUsageBody(body)
}

// parseClaudeUsageBody extracts the five_hour window utilization and reset time
// from a Claude usage response body. It is shared by the proactive probe and
// the reactive management api-call path so both interpret the body identically.
func parseClaudeUsageBody(body []byte) (quotaProbeResult, bool) {
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
	return parseCodexUsageBody(body)
}

// parseCodexUsageBody locates the 5-hour window (limit_window_seconds=18000) in
// a Codex usage response body and returns its used_percent and reset time. It
// is shared by the proactive probe and the reactive management api-call path.
func parseCodexUsageBody(body []byte) (quotaProbeResult, bool) {
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

// parseResetTimestamp parses a reset timestamp from a gjson result. It accepts a
// numeric Unix timestamp (seconds or milliseconds) and several common string
// layouts (RFC3339 with/without fractional seconds and timezone-less ISO-8601),
// mirroring the lenient parsing the UI performs via JavaScript Date. Returns the
// zero time when the value is absent or unparseable.
func parseResetTimestamp(value gjson.Result) time.Time {
	if !value.Exists() {
		return time.Time{}
	}
	if value.Type == gjson.Number {
		return unixTimestamp(value.Int())
	}
	raw := strings.TrimSpace(value.String())
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	} {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts
		}
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return unixTimestamp(n)
	}
	return time.Time{}
}

// unixTimestamp converts a positive Unix timestamp in seconds or milliseconds to
// a time.Time, returning the zero time for non-positive input.
func unixTimestamp(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	// Values past this bound are milliseconds (year ~33658 in seconds).
	if n > 1e12 {
		return time.UnixMilli(n)
	}
	return time.Unix(n, 0)
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

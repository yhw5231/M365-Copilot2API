package web

import (
	"log"
	"os"
	"strconv"
	"time"

	"m365-copilot2api/internal/auth"
)

// 授权账号的续期有两个触发点：进程启动时的一次性扫描，以及请求真正轮到
// 某账号时的惰性刷新（Store.EnsureValid）。两者都救不了"长期闲置"的账号：
// access token 到期后没人去续，账号在后台一直显示离线，而 AAD 的 refresh
// token 本身也有滑动过期窗口（长期不使用即失效，只能重新授权）。
// 这个后台循环补上第三块：按到期时间主动续期，让闲置账号也保持在线。

const (
	// defaultTokenRefreshInterval 是后台检查周期。远小于 access token 的
	// 寿命（约 25–90 分钟），因此每个账号大致每 20–30 分钟被续期一次，
	// 足以让 refresh token 的滑动窗口一直有效。
	defaultTokenRefreshInterval = 5 * time.Minute
	// defaultTokenRefreshLead 是提前续期的提前量：距到期不足该值时即续期，
	// 避免请求正好撞在过期瞬间。
	defaultTokenRefreshLead = 5 * time.Minute
)

// StartTokenRefreshLoop 启动后台定时续期。可用 M365_TOKEN_REFRESH=0 关闭，
// M365_TOKEN_REFRESH_INTERVAL_SECONDS / M365_TOKEN_REFRESH_LEAD_SECONDS 调整
// 周期与提前量。
func (s *Server) StartTokenRefreshLoop() {
	if s.tokens == nil {
		return
	}
	if !envBoolDefault("M365_TOKEN_REFRESH", true) {
		log.Printf("[token-refresh] background loop disabled via M365_TOKEN_REFRESH")
		return
	}
	interval := defaultTokenRefreshInterval
	if v := os.Getenv("M365_TOKEN_REFRESH_INTERVAL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			interval = time.Duration(n) * time.Second
		}
	}
	lead := defaultTokenRefreshLead
	if v := os.Getenv("M365_TOKEN_REFRESH_LEAD_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			lead = time.Duration(n) * time.Second
		}
	}
	log.Printf("[token-refresh] background loop enabled interval=%s lead=%s", interval, lead)
	go func() {
		for {
			time.Sleep(interval)
			s.refreshTokensOnce(lead)
		}
	}()
}

// refreshTokensOnce 续期所有即将到期或已过期的账号，并返回每个账号的结果。
// 处于失败退避窗口内的账号会被 Store.RefreshDue 跳过，因此瞬时故障不会让
// 循环每次都去打 AAD。
func (s *Server) refreshTokensOnce(lead time.Duration) []auth.TokenRefreshResult {
	results := s.tokens.RefreshDue(lead)
	for _, r := range results {
		switch {
		case r.Success:
			log.Printf("[token-refresh] account=%s renewed, expires=%s", r.Email, r.ExpiresAt.Format(time.RFC3339))
		case r.Permanent:
			log.Printf("[token-refresh] account=%s needs re-authorization: %s", r.Email, r.Error)
		default:
			log.Printf("[token-refresh] account=%s transient failure, retrying later: %s", r.Email, r.Error)
		}
	}
	return results
}

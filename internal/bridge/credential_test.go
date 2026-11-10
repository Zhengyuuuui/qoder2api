package bridge

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// --- M1. 凭证错误细分 -------------------------------------------------------

// TOKEN_EXPIRE → 分类为 ErrTypeTokenExpired、HTTP 401。
func TestCredentialTokenExpiredClassification(t *testing.T) {
	cases := []string{
		`{"code":"TOKEN_EXPIRE","message":"token expired"}`,
		`{"code":"401","message":"credential expired, please login again"}`,
		`{"subCode":"TOKEN_EXPIRED","message":"凭证过期"}`,
		`{"message":"invalid_grant: token has expired"}`,
	}
	for _, detail := range cases {
		ue := NewUpstreamError(http.StatusUnauthorized, detail)
		if ue.ErrType != ErrTypeTokenExpired {
			t.Errorf("detail %q: ErrType = %q, want %q", detail, ue.ErrType, ErrTypeTokenExpired)
		}
		if got := ErrorStatus(ue); got != http.StatusUnauthorized {
			t.Errorf("detail %q: status = %d, want 401", detail, got)
		}
		if !strings.Contains(ue.Message, "重新授权") {
			t.Errorf("detail %q: message should mention re-auth: %s", detail, ue.Message)
		}
	}
}

// 额度耗尽 → 分类为 ErrTypeQuotaExhausted、HTTP 403。
func TestCredentialQuotaExhaustedClassification(t *testing.T) {
	cases := []string{
		`{"code":"QUOTA_EXHAUSTED","message":"quota exhausted"}`,
		`{"message":"insufficient_quota for this account"}`,
		`{"message":"额度不足"}`,
	}
	for _, detail := range cases {
		ue := NewUpstreamError(http.StatusForbidden, detail)
		if ue.ErrType != ErrTypeQuotaExhausted {
			t.Errorf("detail %q: ErrType = %q, want %q", detail, ue.ErrType, ErrTypeQuotaExhausted)
		}
		if got := ErrorStatus(ue); got != http.StatusForbidden {
			t.Errorf("detail %q: status = %d, want 403", detail, got)
		}
	}
}

// 试用受限 → 分类为 ErrTypeTrialRestricted。
func TestCredentialTrialRestrictedClassification(t *testing.T) {
	ue := NewUpstreamError(http.StatusForbidden, `{"message":"trial restricted: free trial ended"}`)
	if ue.ErrType != ErrTypeTrialRestricted {
		t.Errorf("ErrType = %q, want %q", ue.ErrType, ErrTypeTrialRestricted)
	}
	if got := ErrorStatus(ue); got != http.StatusForbidden {
		t.Errorf("status = %d, want 403", got)
	}
}

// 模型不可用 → 分类为 ErrTypeModelNotAllowed、消息含「换模型」提示。
func TestCredentialModelNotAllowedClassification(t *testing.T) {
	ue := NewUpstreamError(http.StatusForbidden, `{"message":"model not allowed for this account"}`)
	if ue.ErrType != ErrTypeModelNotAllowed {
		t.Errorf("ErrType = %q, want %q", ue.ErrType, ErrTypeModelNotAllowed)
	}
	if got := ErrorStatus(ue); got != http.StatusForbidden {
		t.Errorf("status = %d, want 403", got)
	}
	if !strings.Contains(ue.Message, "换模型") && !strings.Contains(ue.Message, "改用其他模型") {
		t.Errorf("message should suggest switching model: %s", ue.Message)
	}
}

// --- M2. 差异化处置 ---------------------------------------------------------

// TOKEN_EXPIRE → 账号被标记 needs_reauth、不参与选号。
func TestTokenExpiredMarksNeedsReauthAndExcludesFromPicking(t *testing.T) {
	s := testSlot(t, "a", "t")
	p := NewPool(s)
	b := NewPoolBridge(p, nil)

	before, ok := p.Pick(nil)
	if !ok || before == nil {
		t.Fatal("account should be selectable before the credential error")
	}

	b.failSlot("a", "qfmodel", NewUpstreamError(http.StatusUnauthorized,
		`{"code":"TOKEN_EXPIRE","message":"token expired"}`))

	if !s.NeedsReauth() {
		t.Error("account should be marked needs_reauth")
	}
	if _, ok := p.Pick(nil); ok {
		t.Error("account needing re-auth must not be picked")
	}
	// 状态可见
	st := s.Status()
	if !st.NeedsReauth {
		t.Error("status should expose needs_reauth")
	}
}

// TOKEN_EXPIRE → 不走「等冷却再重试」：不应写账号级冷却时长。
func TestTokenExpiredDoesNotCoolAccount(t *testing.T) {
	s := testSlot(t, "a", "t")
	p := NewPool(s)
	b := NewPoolBridge(p, nil)

	b.failSlot("a", "qfmodel", NewUpstreamError(http.StatusUnauthorized,
		`{"code":"TOKEN_EXPIRE"}`))

	s.mu.Lock()
	cooled := !s.cooldownUntil.IsZero()
	s.mu.Unlock()
	if cooled {
		t.Error("token expiry must not use a timed cooldown (waiting never helps)")
	}
}

// 额度耗尽 → 账号级冷却，且冷却时长明显长于瞬时错误。
func TestQuotaExhaustedLongCooldown(t *testing.T) {
	s := testSlot(t, "a", "t")
	p := NewPool(s)
	b := NewPoolBridge(p, nil)

	b.failSlot("a", "qfmodel", NewUpstreamError(http.StatusForbidden,
		`{"code":"QUOTA_EXHAUSTED","message":"quota exhausted"}`))

	s.mu.Lock()
	rem := time.Until(s.cooldownUntil)
	s.mu.Unlock()
	if rem <= 0 {
		t.Fatal("quota exhaustion should cool the account")
	}
	if rem <= CooldownTransient {
		t.Errorf("quota cooldown %v must be longer than transient %v", rem, CooldownTransient)
	}
	if rem <= CooldownQuota {
		t.Errorf("quota cooldown %v should be longer than the generic quota cooldown %v",
			rem, CooldownQuota)
	}
}

// 模型不可用 → 不换号。
func TestModelNotAllowedDoesNotFailover(t *testing.T) {
	err := NewUpstreamError(http.StatusForbidden, `{"message":"model not allowed"}`)
	if ShouldFailover(err) {
		t.Error("model-not-allowed must not trigger failover")
	}
	if got := ClassifyFail(err); got != FailClassModelNotAllowed {
		t.Errorf("ClassifyFail = %v, want FailClassModelNotAllowed", got)
	}
}

// TOKEN_EXPIRE → 不换号（换过去也是同样过期的凭证）。
func TestTokenExpiredDoesNotFailover(t *testing.T) {
	err := NewUpstreamError(http.StatusUnauthorized, `{"code":"TOKEN_EXPIRE"}`)
	if ShouldFailover(err) {
		t.Error("token expiry must not trigger failover")
	}
}

// 额度耗尽 → 换号（换一个账号可能还有额度）。
func TestQuotaExhaustedDoesFailover(t *testing.T) {
	err := NewUpstreamError(http.StatusForbidden, `{"code":"QUOTA_EXHAUSTED"}`)
	if !ShouldFailover(err) {
		t.Error("quota exhaustion should trigger failover")
	}
}

// --- M3. 重新授权后恢复 -----------------------------------------------------

// 重新授权后 needs_reauth 被清除，账号重新可选。
func TestClearNeedsReauthRestoresAccount(t *testing.T) {
	s := testSlot(t, "a", "t")
	p := NewPool(s)
	b := NewPoolBridge(p, nil)

	b.failSlot("a", "qfmodel", NewUpstreamError(http.StatusUnauthorized, `{"code":"TOKEN_EXPIRE"}`))
	if !s.NeedsReauth() {
		t.Fatal("should be marked needs_reauth")
	}
	if _, ok := p.Pick(nil); ok {
		t.Fatal("account should not be selectable")
	}

	// 重新授权：清除标志
	p.ClearNeedsReauth("a")
	if s.NeedsReauth() {
		t.Error("needs_reauth should be cleared after re-auth")
	}
	if _, ok := p.Pick(nil); !ok {
		t.Error("account should be selectable again after re-auth")
	}
}

// --- M4. 不破坏既有判定顺序 -------------------------------------------------

// 内容审核仍优先于凭证细分（内容审核错误不会被误判成凭证过期）。
func TestContentPolicyStillWins(t *testing.T) {
	ue := NewUpstreamError(500, "InternalError.Algo.DataInspectionFailed: inappropriate content")
	if ue.ErrType != ErrTypeContentPolicy {
		t.Errorf("ErrType = %q, want %q", ue.ErrType, ErrTypeContentPolicy)
	}
	if got := ErrorStatus(ue); got != 400 {
		t.Errorf("status = %d, want 400", got)
	}
}

// 瞬时故障分类不变（5xx 仍是 transient）。
func TestTransientStillClassified(t *testing.T) {
	ue := NewUpstreamError(502, "provider_error")
	if ue.ErrType != ErrTypeTransient {
		t.Errorf("ErrType = %q, want %q", ue.ErrType, ErrTypeTransient)
	}
	if got := ErrorStatus(ue); got != 502 {
		t.Errorf("status = %d, want 502", got)
	}
}

// 客户端参数错仍是 upstream_error（不会误判成凭证类）。
func TestClientParamErrorUnchanged(t *testing.T) {
	ue := NewUpstreamError(400, "invalid_parameter_error: Range of max_tokens")
	if ue.ErrType != ErrTypeUpstream {
		t.Errorf("ErrType = %q, want %q", ue.ErrType, ErrTypeUpstream)
	}
}

// 排队分类不变。
func TestQueueClassificationUnchanged(t *testing.T) {
	detail := nestedResourceLimitDetail(true, true, 3)
	ue := NewUpstreamError(403, detail)
	if ue.ErrType != ErrTypeUpstreamQueue {
		t.Errorf("ErrType = %q, want %q", ue.ErrType, ErrTypeUpstreamQueue)
	}
}

// 裸传输错误不受影响。
func TestTransportErrorUnchanged(t *testing.T) {
	if !ShouldFailover(errors.New("read: connection reset by peer")) {
		t.Error("transport error should still trigger failover")
	}
}

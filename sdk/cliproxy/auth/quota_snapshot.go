package auth

import (
	"context"
	"net/http"
	"time"
)

// ObserveQuotaSnapshot records fresh usage signals without changing cooldown or model state.
func (m *Manager) ObserveQuotaSnapshot(ctx context.Context, authID, provider string, headers http.Header, observedAt time.Time) bool {
	releaseMutation := m.lockAuthMutation(authID)
	defer releaseMutation()
	m.mu.Lock()
	auth := m.auths[authID]
	if auth == nil || observedAt.Before(auth.Quota.ObservedAt) ||
		!auth.Quota.ObserveResponseHeadersForProvider(provider, headers, observedAt) {
		m.mu.Unlock()
		return false
	}
	auth.Generation++
	auth.UpdatedAt = time.Now()
	_ = m.persistLocked(ctx, auth)
	snapshot := auth.Clone()
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuthResult(snapshot, nil, true)
	}
	return true
}

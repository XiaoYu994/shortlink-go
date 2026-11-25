package console

import (
	"net/http"

	"shortlink/internal/httpx"
)

const capabilitiesPath = apiV1Prefix + "/capabilities"

type featureCapabilities struct {
	Analytics bool `json:"analytics"`
	Webhooks  bool `json:"webhooks"`
	RiskScan  bool `json:"risk_scan"`
	Email     bool `json:"email"`
}

/*
capabilitiesResponse 由服务端当前实际挂载的 AuthMethod 与 Module 自动推导（单一事实来源，ADR 0029），
替代旧的 /api/v1/auth/providers，供前端动态渲染登录入口、侧边栏菜单与统计控件。
*/
type capabilitiesResponse struct {
	AuthMethods   []AuthMethodDescriptor `json:"auth_methods"`
	Features      featureCapabilities    `json:"features"`
	PublicBaseURL string                 `json:"public_base_url,omitempty"`
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if httpx.IsClientGone(s.logger, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	methods := make([]AuthMethodDescriptor, 0)
	if s.auth != nil {
		methods = s.auth.descriptors()
	}
	httpx.WriteJSON(s.logger, r.Context(), w, http.StatusOK, capabilitiesResponse{
		AuthMethods: methods,
		Features: featureCapabilities{
			Analytics: s.hasModule(ModuleAnalytics),
			Webhooks:  s.hasModule(ModuleWebhooks),
			RiskScan:  s.riskScanEnabled,
			Email:     s.emailEnabled,
		},
		PublicBaseURL: s.publicBaseURL,
	})
}

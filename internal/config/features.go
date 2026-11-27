package config

import (
	"fmt"
	"log/slog"
)

/*
FeatureFlags 控制纯内置、无独立外部凭证的可选业务模块开关（ADR 0029）。
默认全开（true），保证开箱即用的完整体验；需要极简部署时在 YAML 或环境变量设为 false。
依赖外部凭证或基础设施的模块（REDIS_URL、WEBHOOK_SECRET_KEY、smtp.host、OIDC/GitHub/PhoneOTP、console.web_dist_dir）
直接以对应配置存在作为开关，不在此重复声明布尔值。
*/
type FeatureFlags struct {
	Analytics bool // features.analytics / FEATURE_ANALYTICS：点击统计记录、统计查询与日表分区治理
	RiskScan  bool // features.risk_scan / FEATURE_RISK_SCAN：短链创建/改 URL 后的异步风控扫描
}

type featuresSection struct {
	Analytics *bool `yaml:"analytics"`
	RiskScan  *bool `yaml:"risk_scan"`
}

// DefaultFeatureFlags 返回默认全开的内置特性开关。
func DefaultFeatureFlags() FeatureFlags {
	return FeatureFlags{
		Analytics: true,
		RiskScan:  true,
	}
}

// LoadFeatureFlags 按默认值 < YAML features 节 < FEATURE_* 环境变量加载并校验内置特性开关。
func LoadFeatureFlags(file *File) (FeatureFlags, error) {
	flags := DefaultFeatureFlags()
	if file != nil {
		var sec featuresSection
		if err := DecodeSection(file.Features, &sec); err != nil {
			return FeatureFlags{}, fmt.Errorf("features: %w", err)
		}
		if sec.Analytics != nil {
			flags.Analytics = *sec.Analytics
		}
		if sec.RiskScan != nil {
			flags.RiskScan = *sec.RiskScan
		}
	}
	var err error
	if flags.Analytics, err = EnvBool("FEATURE_ANALYTICS", flags.Analytics); err != nil {
		return FeatureFlags{}, err
	}
	if flags.RiskScan, err = EnvBool("FEATURE_RISK_SCAN", flags.RiskScan); err != nil {
		return FeatureFlags{}, err
	}
	return flags, nil
}

// LogValue 格式化特性开关，供各进程启动日志统一输出。
func (f FeatureFlags) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("analytics", f.Analytics),
		slog.Bool("risk_scan", f.RiskScan),
	)
}

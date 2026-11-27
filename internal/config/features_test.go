package config

import "testing"

func TestLoadFeatureFlagsDefaults(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	t.Setenv("FEATURE_ANALYTICS", "")
	t.Setenv("FEATURE_RISK_SCAN", "")

	got, err := LoadFeatureFlags(nil)
	if err != nil {
		t.Fatalf("LoadFeatureFlags: %v", err)
	}
	if !got.Analytics || !got.RiskScan {
		t.Fatalf("flags = %+v, want both true by default", got)
	}
}

func TestLoadFeatureFlagsYAMLAndEnvOverride(t *testing.T) {
	t.Setenv(EnvConfigPath, "")
	t.Setenv("FEATURE_ANALYTICS", "true")
	t.Setenv("FEATURE_RISK_SCAN", "")

	f := loadFileFromYAML(t, "features:\n  analytics: false\n  risk_scan: false\n")
	got, err := LoadFeatureFlags(f)
	if err != nil {
		t.Fatalf("LoadFeatureFlags: %v", err)
	}
	if !got.Analytics || got.RiskScan {
		t.Fatalf("flags = %+v, want Analytics=true (env override) and RiskScan=false (yaml)", got)
	}
}

func TestLoadFeatureFlagsInvalid(t *testing.T) {
	cases := []struct {
		name string
		env  string
		val  string
	}{
		{name: "invalid analytics", env: "FEATURE_ANALYTICS", val: "not-a-bool"},
		{name: "invalid risk scan", env: "FEATURE_RISK_SCAN", val: "yes-please"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvConfigPath, "")
			t.Setenv("FEATURE_ANALYTICS", "")
			t.Setenv("FEATURE_RISK_SCAN", "")
			t.Setenv(tc.env, tc.val)
			if _, err := LoadFeatureFlags(nil); err == nil {
				t.Fatalf("LoadFeatureFlags(%s=%q) succeeded, want error", tc.env, tc.val)
			}
		})
	}
}

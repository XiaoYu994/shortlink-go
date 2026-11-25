package config

import "testing"

func TestLoadFeatureFlagsDefaults(t *testing.T) {
	t.Setenv("FEATURE_ANALYTICS", "")
	t.Setenv("FEATURE_RISK_SCAN", "")

	got, err := LoadFeatureFlags()
	if err != nil {
		t.Fatalf("LoadFeatureFlags: %v", err)
	}
	if !got.Analytics || !got.RiskScan {
		t.Fatalf("flags = %+v, want both true by default", got)
	}
}

func TestLoadFeatureFlagsExplicit(t *testing.T) {
	t.Setenv("FEATURE_ANALYTICS", "false")
	t.Setenv("FEATURE_RISK_SCAN", "false")

	got, err := LoadFeatureFlags()
	if err != nil {
		t.Fatalf("LoadFeatureFlags: %v", err)
	}
	if got.Analytics || got.RiskScan {
		t.Fatalf("flags = %+v, want both false", got)
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
			t.Setenv("FEATURE_ANALYTICS", "")
			t.Setenv("FEATURE_RISK_SCAN", "")
			t.Setenv(tc.env, tc.val)
			if _, err := LoadFeatureFlags(); err == nil {
				t.Fatalf("LoadFeatureFlags(%s=%q) succeeded, want error", tc.env, tc.val)
			}
		})
	}
}

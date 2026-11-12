package config

import (
	"strings"
	"testing"
	"time"

	"shortlink/internal/postgres"
)

func TestLoadConfig(t *testing.T) {
	// make test 会导出 .env。每个子测试都要把 LoadConfig 读取的变量设一遍，空字符串表示未配置。
	base := map[string]string{
		"ADDR":                  "",
		"DATABASE_URL":          "postgres://localhost/shortlink",
		"DB_MAX_OPEN_CONNS":     "",
		"DB_MAX_IDLE_CONNS":     "",
		"DB_CONN_MAX_LIFETIME":  "",
		"DB_CONN_MAX_IDLE_TIME": "",
		"REQUEST_TIMEOUT":       "",
		"SHUTDOWN_TIMEOUT":      "",
		"REDIS_URL":             "redis://localhost:6379/0",
	}
	defaults := Config{
		Addr:            defaultAddr,
		DatabaseURL:     "postgres://localhost/shortlink",
		RedisURL:        "redis://localhost:6379/0",
		Pool:            postgres.DefaultPoolConfig(),
		RequestTimeout:  defaultRequestTimeout,
		ShutdownTimeout: defaultShutdownTimeout,
	}

	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr string
	}{
		{name: "defaults", want: defaults},
		{
			name: "explicit values",
			env: map[string]string{
				"ADDR":                  ":9090",
				"DATABASE_URL":          "postgres://user:pass@db:5432/shortlink?sslmode=disable",
				"DB_MAX_OPEN_CONNS":     "20",
				"DB_MAX_IDLE_CONNS":     "4",
				"DB_CONN_MAX_LIFETIME":  "1h30m",
				"DB_CONN_MAX_IDLE_TIME": "5m",
				"REQUEST_TIMEOUT":       "2s",
				"SHUTDOWN_TIMEOUT":      "15s",
				"REDIS_URL":             "rediss://user:pass@redis:6380/1",
			},
			want: Config{
				Addr:        ":9090",
				DatabaseURL: "postgres://user:pass@db:5432/shortlink?sslmode=disable",
				RedisURL:    "rediss://user:pass@redis:6380/1",
				Pool: postgres.PoolConfig{
					MaxOpenConns:    20,
					MaxIdleConns:    4,
					ConnMaxLifetime: 90 * time.Minute,
					ConnMaxIdleTime: 5 * time.Minute,
				},
				RequestTimeout:  2 * time.Second,
				ShutdownTimeout: 15 * time.Second,
			},
		},
		{
			name: "idle conns equal open conns",
			env: map[string]string{
				"DB_MAX_OPEN_CONNS": "10",
				"DB_MAX_IDLE_CONNS": "10",
			},
			want: Config{
				Addr:        defaultAddr,
				DatabaseURL: "postgres://localhost/shortlink",
				RedisURL:    "redis://localhost:6379/0",
				Pool: postgres.PoolConfig{
					MaxOpenConns:    10,
					MaxIdleConns:    10,
					ConnMaxLifetime: defaults.Pool.ConnMaxLifetime,
					ConnMaxIdleTime: defaults.Pool.ConnMaxIdleTime,
				},
				RequestTimeout:  defaultRequestTimeout,
				ShutdownTimeout: defaultShutdownTimeout,
			},
		},
		{
			name:    "missing database url",
			env:     map[string]string{"DATABASE_URL": ""},
			wantErr: "DATABASE_URL is required",
		},
		{
			name:    "missing redis url",
			env:     map[string]string{"REDIS_URL": ""},
			wantErr: "REDIS_URL is required",
		},
		{
			name:    "invalid open conns",
			env:     map[string]string{"DB_MAX_OPEN_CONNS": "abc"},
			wantErr: "DB_MAX_OPEN_CONNS",
		},
		{
			name:    "open conns not positive",
			env:     map[string]string{"DB_MAX_OPEN_CONNS": "0"},
			wantErr: "DB_MAX_OPEN_CONNS must be positive",
		},
		{
			name: "idle conns above open conns",
			env: map[string]string{
				"DB_MAX_OPEN_CONNS": "10",
				"DB_MAX_IDLE_CONNS": "11",
			},
			wantErr: "DB_MAX_IDLE_CONNS must be between 0 and DB_MAX_OPEN_CONNS",
		},
		{
			name:    "negative conn lifetime",
			env:     map[string]string{"DB_CONN_MAX_LIFETIME": "-1s"},
			wantErr: "connection lifetimes must not be negative",
		},
		{
			name:    "invalid shutdown timeout",
			env:     map[string]string{"SHUTDOWN_TIMEOUT": "abc"},
			wantErr: "SHUTDOWN_TIMEOUT",
		},
		{
			name:    "invalid request timeout",
			env:     map[string]string{"REQUEST_TIMEOUT": "abc"},
			wantErr: "REQUEST_TIMEOUT",
		},
		{
			name:    "zero request timeout",
			env:     map[string]string{"REQUEST_TIMEOUT": "0s"},
			wantErr: "REQUEST_TIMEOUT must be positive",
		},
		{
			name:    "zero shutdown timeout",
			env:     map[string]string{"SHUTDOWN_TIMEOUT": "0s"},
			wantErr: "SHUTDOWN_TIMEOUT must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applyEnv(t, base, tt.env)

			got, err := LoadConfig()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadConfig() error = nil, want substring %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("LoadConfig() error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig() returned error: %v", err)
			}
			if got != tt.want {
				t.Errorf("LoadConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// applyEnv 先铺上 base，再用 override 覆盖。子测试结束时 t.Setenv 会恢复原值。
func applyEnv(t *testing.T, base, override map[string]string) {
	t.Helper()
	for k, v := range base {
		t.Setenv(k, v)
	}
	for k, v := range override {
		t.Setenv(k, v)
	}
}

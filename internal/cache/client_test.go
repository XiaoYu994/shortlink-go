package cache

import (
	"testing"
	"time"
)

func TestNewOptions(t *testing.T) {
	tests := []struct {
		name     string
		redisURL string
		wantAddr string
		wantUser string
		wantPass string
		wantDB   int
		wantTLS  bool
		wantErr  bool
	}{
		{
			name:     "no password",
			redisURL: "redis://localhost:6379/0",
			wantAddr: "localhost:6379",
		},
		{
			name:     "auth and db",
			redisURL: "redis://user:pass@redis.example.com:6380/3",
			wantAddr: "redis.example.com:6380",
			wantUser: "user",
			wantPass: "pass",
			wantDB:   3,
		},
		{
			name:     "tls",
			redisURL: "rediss://redis.example.com:6380",
			wantAddr: "redis.example.com:6380",
			wantTLS:  true,
		},
		{
			name:     "no db uses zero",
			redisURL: "redis://localhost:6379",
			wantAddr: "localhost:6379",
		},
		{
			name:     "invalid scheme",
			redisURL: "http://redis.example.com",
			wantErr:  true,
		},
		{
			name:     "empty",
			redisURL: "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := newOptions(tt.redisURL)
			if tt.wantErr {
				if err == nil {
					t.Fatal("newOptions() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("newOptions(): %v", err)
			}

			if opts.Addr != tt.wantAddr {
				t.Errorf("Addr = %q, want %q", opts.Addr, tt.wantAddr)
			}
			if opts.Username != tt.wantUser {
				t.Errorf("Username = %q, want %q", opts.Username, tt.wantUser)
			}
			if opts.Password != tt.wantPass {
				t.Errorf("Password = %q, want %q", opts.Password, tt.wantPass)
			}
			if opts.DB != tt.wantDB {
				t.Errorf("DB = %d, want %d", opts.DB, tt.wantDB)
			}
			if got := opts.TLSConfig != nil; got != tt.wantTLS {
				t.Errorf("TLSConfig set = %v, want %v", got, tt.wantTLS)
			}
			if opts.DialTimeout != 500*time.Millisecond {
				t.Errorf("DialTimeout = %v, want %v", opts.DialTimeout, 500*time.Millisecond)
			}
			if opts.ReadTimeout != 200*time.Millisecond {
				t.Errorf("ReadTimeout = %v, want %v", opts.ReadTimeout, 200*time.Millisecond)
			}
			if opts.WriteTimeout != 200*time.Millisecond {
				t.Errorf("WriteTimeout = %v, want %v", opts.WriteTimeout, 200*time.Millisecond)
			}
			if opts.MaxRetries != 1 {
				t.Errorf("MaxRetries = %d, want 1", opts.MaxRetries)
			}
			if opts.DialerRetries != 1 {
				t.Errorf("DialerRetries = %d, want 1", opts.DialerRetries)
			}
		})
	}
}

// TestNewClientDoesNotDial Redis 不可达时也能建出客户端，失败留给第一次命令。
func TestNewClientDoesNotDial(t *testing.T) {
	client, err := NewClient("redis://127.0.0.1:1/0")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if client == nil {
		t.Fatal("NewClient returned a nil client")
	}
}

func TestNewClientRejectsBadURL(t *testing.T) {
	for _, redisURL := range []string{"", "http://redis.example.com"} {
		if _, err := NewClient(redisURL); err == nil {
			t.Errorf("NewClient(%q) error = nil, want error", redisURL)
		}
	}
}

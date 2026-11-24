//go:build devtest

package main

import (
	"bytes"
	"testing"
)

func TestParseWorkerArgsDevTestFlags(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantOK    bool
		wantErr   bool
		wantHosts []string
	}{
		{
			name:      "valid loopback test host",
			args:      []string{"--allow-test-host", "127.0.0.1:18089"},
			wantOK:    true,
			wantHosts: []string{"127.0.0.1:18089"},
		},
		{
			name:      "valid localhost test host",
			args:      []string{"--allow-test-host", "localhost:18089"},
			wantOK:    true,
			wantHosts: []string{"localhost:18089"},
		},
		{
			name:    "rejects public ip",
			args:    []string{"--allow-test-host", "8.8.8.8:80"},
			wantOK:  false,
			wantErr: true,
		},
		{
			name:    "rejects missing port",
			args:    []string{"--allow-test-host", "127.0.0.1"},
			wantOK:  false,
			wantErr: true,
		},
		{
			name:    "rejects non-numeric port",
			args:    []string{"--allow-test-host", "127.0.0.1:abc"},
			wantOK:  false,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			opts, ok, err := parseWorkerArgs(tt.args, &stderr)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (err=%v)", ok, tt.wantOK, err)
			}
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(opts.allowTestHosts) != len(tt.wantHosts) {
				t.Fatalf("allowTestHosts = %v, want %v", opts.allowTestHosts, tt.wantHosts)
			}
			for i := range tt.wantHosts {
				if opts.allowTestHosts[i] != tt.wantHosts[i] {
					t.Errorf("allowTestHosts[%d] = %q, want %q", i, opts.allowTestHosts[i], tt.wantHosts[i])
				}
			}
		})
	}
}

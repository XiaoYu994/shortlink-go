package identity

import (
	"errors"
	"strings"
	"testing"
)

func TestExternalIdentityNormalize(t *testing.T) {
	tests := []struct {
		name    string
		in      ExternalIdentity
		want    ExternalIdentity
		wantErr bool
	}{
		{
			name: "normalizes email and name",
			in: ExternalIdentity{
				Issuer:        "http://127.0.0.1:5556/dex",
				Subject:       "sub-1",
				Email:         "  Alice@Example.COM ",
				EmailVerified: true,
				Name:          "  Alice  ",
			},
			want: ExternalIdentity{
				Issuer:        "http://127.0.0.1:5556/dex",
				Subject:       "sub-1",
				Email:         "alice@example.com",
				EmailVerified: true,
				Name:          "Alice",
			},
		},
		{
			name: "empty email cannot be verified",
			in: ExternalIdentity{
				Issuer:        "http://127.0.0.1:5556/dex",
				Subject:       "sub-2",
				Email:         "   ",
				EmailVerified: true,
			},
			want: ExternalIdentity{
				Issuer:        "http://127.0.0.1:5556/dex",
				Subject:       "sub-2",
				Email:         "",
				EmailVerified: false,
			},
		},
		{
			name:    "empty issuer rejected",
			in:      ExternalIdentity{Subject: "sub-1"},
			wantErr: true,
		},
		{
			name:    "padded issuer rejected",
			in:      ExternalIdentity{Issuer: " https://idp.example.com", Subject: "sub-1"},
			wantErr: true,
		},
		{
			name:    "empty subject rejected",
			in:      ExternalIdentity{Issuer: "https://idp.example.com"},
			wantErr: true,
		},
		{
			name:    "overlong subject rejected",
			in:      ExternalIdentity{Issuer: "https://idp.example.com", Subject: strings.Repeat("a", maxSubjectBytes+1)},
			wantErr: true,
		},
		{
			name:    "overlong email rejected",
			in:      ExternalIdentity{Issuer: "https://idp.example.com", Subject: "sub-1", Email: strings.Repeat("a", maxEmailBytes+1)},
			wantErr: true,
		},
		{
			name:    "overlong name rejected",
			in:      ExternalIdentity{Issuer: "https://idp.example.com", Subject: "sub-1", Name: strings.Repeat("a", maxNameBytes+1)},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.in.Normalize()
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidExternalIdentity) {
					t.Fatalf("err = %v, want ErrInvalidExternalIdentity", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if got != tt.want {
				t.Errorf("got = %+v, want %+v", got, tt.want)
			}
		})
	}
}

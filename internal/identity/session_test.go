package identity

import (
	"errors"
	"strings"
	"testing"
)

func TestOIDCIdentityNormalize(t *testing.T) {
	tests := []struct {
		name    string
		in      OIDCIdentity
		want    OIDCIdentity
		wantErr bool
	}{
		{
			name: "normalizes email and name",
			in: OIDCIdentity{
				Issuer:        "http://127.0.0.1:5556/dex",
				Subject:       "sub-1",
				Email:         "  Alice@Example.COM ",
				EmailVerified: true,
				Name:          "  Alice  ",
			},
			want: OIDCIdentity{
				Issuer:        "http://127.0.0.1:5556/dex",
				Subject:       "sub-1",
				Email:         "alice@example.com",
				EmailVerified: true,
				Name:          "Alice",
			},
		},
		{
			name: "empty email cannot be verified",
			in: OIDCIdentity{
				Issuer:        "http://127.0.0.1:5556/dex",
				Subject:       "sub-2",
				Email:         "   ",
				EmailVerified: true,
			},
			want: OIDCIdentity{
				Issuer:        "http://127.0.0.1:5556/dex",
				Subject:       "sub-2",
				Email:         "",
				EmailVerified: false,
			},
		},
		{
			name:    "empty issuer rejected",
			in:      OIDCIdentity{Subject: "sub-1"},
			wantErr: true,
		},
		{
			name:    "padded issuer rejected",
			in:      OIDCIdentity{Issuer: " https://idp.example.com", Subject: "sub-1"},
			wantErr: true,
		},
		{
			name:    "empty subject rejected",
			in:      OIDCIdentity{Issuer: "https://idp.example.com"},
			wantErr: true,
		},
		{
			name:    "overlong subject rejected",
			in:      OIDCIdentity{Issuer: "https://idp.example.com", Subject: strings.Repeat("a", maxSubjectBytes+1)},
			wantErr: true,
		},
		{
			name:    "overlong email rejected",
			in:      OIDCIdentity{Issuer: "https://idp.example.com", Subject: "sub-1", Email: strings.Repeat("a", maxEmailBytes+1)},
			wantErr: true,
		},
		{
			name:    "overlong name rejected",
			in:      OIDCIdentity{Issuer: "https://idp.example.com", Subject: "sub-1", Name: strings.Repeat("a", maxNameBytes+1)},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.in.Normalize()
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidOIDCIdentity) {
					t.Fatalf("err = %v, want ErrInvalidOIDCIdentity", err)
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

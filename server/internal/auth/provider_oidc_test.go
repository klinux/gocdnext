package auth_test

import (
	"context"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/auth"
)

func TestOIDCProvider_ValidationAtBoot(t *testing.T) {
	cases := []struct {
		name string
		cfg  auth.OIDCConfig
	}{
		{"empty", auth.OIDCConfig{}},
		{"missing secret", auth.OIDCConfig{
			Issuer:      "https://accounts.google.com",
			ClientID:    "x",
			CallbackURL: "https://ci.example.com/cb",
		}},
		{"missing callback", auth.OIDCConfig{
			Issuer:       "https://accounts.google.com",
			ClientID:     "x",
			ClientSecret: "y",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := auth.NewOIDCProvider(context.Background(), tc.cfg); err == nil {
				t.Fatalf("want error, got nil")
			}
		})
	}
}

package main

import "testing"

func TestWithDefaultPort(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "domain without port",
			in:   "example.com",
			want: "example.com:443",
		},
		{
			name: "domain with port",
			in:   "example.com:8443",
			want: "example.com:8443",
		},
		{
			name: "ipv6 without port",
			in:   "2001:db8::1",
			want: "[2001:db8::1]:443",
		},
		{
			name: "bracketed ipv6 without port",
			in:   "[2001:db8::1]",
			want: "[2001:db8::1]:443",
		},
		{
			name: "ipv6 with port",
			in:   "[2001:db8::1]:8443",
			want: "[2001:db8::1]:8443",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withDefaultPort(tt.in, "443")
			if got != tt.want {
				t.Fatalf("withDefaultPort(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

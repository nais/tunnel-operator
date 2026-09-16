package main

import "testing"

func TestTargetAddress(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		port int
		want string
	}{
		{name: "IPv4", ip: "10.0.0.10", port: 5432, want: "10.0.0.10:5432"},
		{name: "IPv6", ip: "2001:db8::10", port: 5432, want: "[2001:db8::10]:5432"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := targetAddress(tt.ip, tt.port); got != tt.want {
				t.Errorf("targetAddress(%q, %d) = %q, want %q", tt.ip, tt.port, got, tt.want)
			}
		})
	}
}

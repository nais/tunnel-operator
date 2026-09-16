package main

import "testing"

func TestTargetAddress(t *testing.T) {
	tests := []struct {
		name string
		host string
		ip   string
		port int
		want string
	}{
		{name: "external IPv4", host: "postgres.example.net", ip: "10.0.0.10", port: 5432, want: "10.0.0.10:5432"},
		{name: "external IPv6", host: "postgres.example.net", ip: "2001:db8::10", port: 5432, want: "[2001:db8::10]:5432"},
		{
			name: "in-cluster service", host: "pg-rw.basseng.svc.cluster.local", port: 5432,
			want: "pg-rw.basseng.svc.cluster.local:5432",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := targetAddress(tt.host, tt.ip, tt.port); got != tt.want {
				t.Errorf("targetAddress(%q, %q, %d) = %q, want %q", tt.host, tt.ip, tt.port, got, tt.want)
			}
		})
	}
}

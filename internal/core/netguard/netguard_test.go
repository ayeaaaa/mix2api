package netguard

import (
	"net"
	"testing"
)

func TestIsPublicIPRejectsSpecialRanges(t *testing.T) {
	for _, value := range []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "100.64.0.1",
		"169.254.1.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1",
		"224.0.0.1", "240.0.0.1", "0.0.0.0", "::1", "fc00::1", "fe80::1",
		"2001:db8::1", "ff02::1",
	} {
		if IsPublicIP(net.ParseIP(value)) {
			t.Fatalf("accepted %s", value)
		}
	}
	for _, value := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !IsPublicIP(net.ParseIP(value)) {
			t.Fatalf("rejected public %s", value)
		}
	}
}

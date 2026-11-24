package ocm

import (
	"bufio"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func gatewayProfile(t *testing.T, host string) Profile {
	t.Helper()
	profile, err := parseProfile(bufio.NewScanner(strings.NewReader(
		"OC_HOST="+host+"\nOC_USER=test\nOC_PASS=test\nOC_DNS_DOMAINS=corp.example\nOC_DNS_SERVER=192.0.2.53\n")), "/test/hook")
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestGatewayDNSOverlap(t *testing.T) {
	for host, want := range map[string]bool{
		"https://VPN.Corp.Example.:8443/group": true,
		"corp.example":                         true,
		"notcorp.example":                      false,
		"corp.example.other":                   false,
		"192.0.2.99":                           false,
	} {
		t.Run(host, func(t *testing.T) {
			if got := gatewayProfile(t, host).gatewayNeedsUplinkDNS(); got != want {
				t.Fatalf("gateway overlap = %t, want %t", got, want)
			}
		})
	}
}

func TestGatewayDNSUsesUplinkAndRefreshesOnLaunch(t *testing.T) {
	url := "https://vpn.corp.example:8443/group"
	profile := gatewayProfile(t, url)
	original := slices.Clone(profile.args)
	address := "198.51.100.5"
	var queried []string
	transport := Transport{command: func(ctx context.Context, input, path string, args ...string) (string, error) {
		switch {
		case strings.HasPrefix(input, "list "):
			return "subKey [0] = State:/Network/Service/AAAA/IPv4\nsubKey [1] = State:/Network/Service/BBBB/IPv4", nil
		case strings.Contains(input, "/AAAA/IPv4"):
			return "  InterfaceName : utun6\n", nil
		case strings.Contains(input, "/BBBB/IPv4"):
			return "  InterfaceName : en0\n", nil
		case strings.Contains(input, "/BBBB/DNS"):
			return "  ServerAddresses : <array> {\n  0 : 192.0.2.1\n  1 : 192.0.2.2\n}", nil
		case path == "/usr/bin/dig":
			queried = append(queried, args[0])
			if args[1] != "vpn.corp.example." {
				t.Fatalf("unexpected query: %v", args)
			}
			if args[0] == "@192.0.2.1" {
				return "", errors.New("unavailable")
			}
			return "vpn.corp.example. 60 IN CNAME front.example.\nfront.example. 60 IN A " + address, nil
		default:
			t.Fatalf("unexpected command: %q %q %v", input, path, args)
			return "", nil
		}
	}}
	for _, ip := range []string{"198.51.100.5", "198.51.100.6"} {
		address = ip
		args, err := transport.gatewayArgs(context.Background(), profile, "en0")
		if err != nil || !slices.Equal(args[len(args)-3:], []string{"--resolve=vpn.corp.example:" + ip, "--", url}) {
			t.Fatalf("gateway arguments = %v, err = %v", args, err)
		}
	}
	if !slices.Equal(profile.args, original) || !reflect.DeepEqual(queried, []string{"@192.0.2.1", "@192.0.2.2", "@192.0.2.1", "@192.0.2.2"}) {
		t.Fatalf("profile args changed or wrong DNS servers queried: %v", queried)
	}
}

func TestGatewayDNSFailureAndCancellation(t *testing.T) {
	profile := gatewayProfile(t, "vpn.corp.example")
	transport := Transport{command: func(ctx context.Context, input, path string, args ...string) (string, error) {
		switch path {
		case "/usr/sbin/ipconfig":
			return "192.0.2.1", nil
		case "/usr/bin/dig":
			return "", nil
		default:
			return "", nil
		}
	}}
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			cancel()
		}
		args, err := transport.gatewayArgs(ctx, profile, "en0")
		cancel()
		if args != nil || !errors.Is(err, errGatewayDNS) || (cancelled && !errors.Is(err, context.Canceled)) {
			t.Fatalf("cancelled=%t: args=%v err=%v", cancelled, args, err)
		}
		if !cancelled && !strings.Contains(err.Error(), "192.0.2.1") {
			t.Fatal(err)
		}
	}
}

func TestGatewayDNSFailureStatus(t *testing.T) {
	status := Status{State: "gateway-dns-failed", Profile: "test", SavedProfile: "test"}
	data, err := status.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"state":"failed"`, `"stage":"gateway-dns-failed"`, `"error":"gateway DNS lookup failed"`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("missing %s in %s", field, data)
		}
	}
}

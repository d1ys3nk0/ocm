package ocm

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var profileName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var certificate = regexp.MustCompile(`^[A-Za-z0-9:+=/.\-]+$`)

type Profile struct {
	password string
	args     []string
	dns      dnsPolicy
}

func LoadProfile(root, name, hook string) (Profile, error) {
	if !profileName.MatchString(name) {
		return Profile{}, fmt.Errorf("invalid profile name")
	}
	path := filepath.Join(root, "profiles", name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ownedByRoot(info) {
		return Profile{}, fmt.Errorf("profile must be a root-owned regular file with mode 0600")
	}
	file, err := os.Open(path)
	if err != nil {
		return Profile{}, fmt.Errorf("cannot open profile")
	}
	defer file.Close()
	return parseProfile(bufio.NewScanner(file), hook)
}

func parseProfile(scanner *bufio.Scanner, hook string) (Profile, error) {
	values := map[string]string{
		"OC_RECONNECT_TIMEOUT": "300", "OC_DPD_INTERVAL": "30",
		"OC_VERBOSE": "0", "OC_COMPRESSION": "stateless",
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.HasPrefix(key, "OC_") {
			values[key] = value
		}
	}
	if scanner.Err() != nil {
		return Profile{}, fmt.Errorf("cannot read profile")
	}
	host := values["OC_HOST"]
	address := host
	if !strings.Contains(address, "://") {
		address = "https://" + address
	}
	server, err := url.Parse(address)
	if err != nil || server.Scheme != "https" || server.Hostname() == "" || server.User != nil || strings.HasPrefix(host, "-") {
		return Profile{}, fmt.Errorf("invalid OC_HOST")
	}
	if values["OC_USER"] == "" || values["OC_PASS"] == "" {
		return Profile{}, fmt.Errorf("OC_USER and OC_PASS are required")
	}
	args := []string{"--non-inter", "--passwd-on-stdin", "--no-proxy", "--timestamp",
		"--user=" + values["OC_USER"], "--authgroup=" + values["OC_GROUP"], "--script", hook}
	for _, field := range []struct {
		key, flag string
		min, max  int
	}{
		{"OC_RECONNECT_TIMEOUT", "--reconnect-timeout", 1, 86400},
		{"OC_DPD_INTERVAL", "--force-dpd", 1, 3600},
		{"OC_MTU", "--mtu", 576, 65535},
		{"OC_BASE_MTU", "--base-mtu", 576, 65535},
		{"OC_DTLS_LOCAL_PORT", "--dtls-local-port", 1, 65535},
	} {
		value := values[field.key]
		if value == "" && field.key != "OC_RECONNECT_TIMEOUT" && field.key != "OC_DPD_INTERVAL" {
			continue
		}
		number, err := strconv.Atoi(value)
		if err != nil || number < field.min || number > field.max || strings.HasPrefix(value, "+") {
			return Profile{}, fmt.Errorf("invalid %s", field.key)
		}
		args = append(args, field.flag+"="+strconv.Itoa(number))
	}
	switch values["OC_VERBOSE"] {
	case "1":
		args = append(args, "--verbose")
	case "0":
	default:
		return Profile{}, fmt.Errorf("invalid OC_VERBOSE")
	}
	switch values["OC_COMPRESSION"] {
	case "none":
		args = append(args, "--no-deflate")
	case "all":
		args = append(args, "--deflate")
	case "stateless":
	default:
		return Profile{}, fmt.Errorf("invalid OC_COMPRESSION")
	}
	if pin := values["OC_SERVERCERT"]; pin != "" {
		if !certificate.MatchString(pin) {
			return Profile{}, fmt.Errorf("invalid OC_SERVERCERT")
		}
		args = append(args, "--servercert="+pin)
	}
	dns, err := parseDNSPolicy(values["OC_DNS_DOMAINS"], values["OC_DNS_SERVER"])
	if err != nil {
		return Profile{}, err
	}
	return Profile{password: values["OC_PASS"], args: append(args, "--", host), dns: dns}, nil
}

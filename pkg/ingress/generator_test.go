package ingress

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"quartermaster/pkg/types"
)

func TestGenerateCaddyfile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ingress-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	origCaddy := caddyfilePath
	origHosts := hostsFilePath
	caddyfilePath = filepath.Join(tmpDir, "Caddyfile")
	hostsFilePath = filepath.Join(tmpDir, "hosts")
	defer func() {
		caddyfilePath = origCaddy
		hostsFilePath = origHosts
	}()

	services := []ServiceEntry{
		{Name: "jellyfin", IP: net.ParseIP("10.42.0.10"), Ingress: &types.IngressConfig{Host: "media.example.com", Port: 8096}},
		{Name: "sonarr", IP: net.ParseIP("10.42.0.11"), Ingress: &types.IngressConfig{Host: "tv.example.com", Port: 8989, Auth: true}, AutheliaIP: "10.42.0.99"},
		{Name: "db", IP: net.ParseIP("10.42.0.13")},
	}

	if err := generateCaddyfile(services, "", "internal"); err != nil {
		t.Fatalf("generateCaddyfile: %v", err)
	}
	if err := generateHosts(services); err != nil {
		t.Fatalf("generateHosts: %v", err)
	}

	caddyData, _ := os.ReadFile(caddyfilePath)
	caddy := string(caddyData)

	if !strings.Contains(caddy, "localhost {") {
		t.Error("missing localhost server block for internal mode")
	}
	if !strings.Contains(caddy, "reverse_proxy 10.42.0.10:8096") {
		t.Error("missing direct IP reverse_proxy")
	}
	if !strings.Contains(caddy, "tls internal") {
		t.Error("missing tls internal")
	}
	if !strings.Contains(caddy, fmt.Sprintf("forward_auth 10.42.0.99:9091")) {
		t.Error("missing forward_auth with authelia IP")
	}

	hostsData, _ := os.ReadFile(hostsFilePath)
	hosts := string(hostsData)

	if !strings.Contains(hosts, "jellyfin") {
		t.Error("hosts file missing jellyfin")
	}
	if !strings.Contains(hosts, "db") {
		t.Error("hosts file missing db")
	}
}

func TestGenerateEmpty(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ingress-empty-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	origCaddy := caddyfilePath
	origHosts := hostsFilePath
	caddyfilePath = filepath.Join(tmpDir, "Caddyfile")
	hostsFilePath = filepath.Join(tmpDir, "hosts")
	defer func() {
		caddyfilePath = origCaddy
		hostsFilePath = origHosts
	}()

	if err := generateCaddyfile(nil, "", "internal"); err != nil {
		t.Fatal(err)
	}
	if err := generateHosts(nil); err != nil {
		t.Fatal(err)
	}

	caddyData, _ := os.ReadFile(caddyfilePath)
	if !strings.Contains(string(caddyData), "no ingress configured") {
		t.Error("empty config should show placeholder message")
	}
}

func TestLetsEncryptDomainSuffix(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ingress-le-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	origCaddy := caddyfilePath
	caddyfilePath = filepath.Join(tmpDir, "Caddyfile")
	defer func() { caddyfilePath = origCaddy }()

	services := []ServiceEntry{
		{Name: "jellyfin", IP: net.ParseIP("10.42.0.10"), Ingress: &types.IngressConfig{Host: "media", Port: 8096}},
	}

	if err := generateCaddyfile(services, "example.com", "letsencrypt"); err != nil {
		t.Fatal(err)
	}
	caddyData, _ := os.ReadFile(caddyfilePath)
	caddy := string(caddyData)

	if !strings.Contains(caddy, "media.example.com {") {
		t.Error("short host should get domain suffix in letsencrypt mode")
	}
	if strings.Contains(caddy, "tls internal") {
		t.Error("letsencrypt mode should NOT have tls internal")
	}
}

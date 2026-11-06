// Package tailnet reads device information from the local Tailscale daemon.
package tailnet

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Device is one machine in the tailnet.
type Device struct {
	Name    string `json:"name"`
	DNSName string `json:"dnsName"`
	IP      string `json:"ip"`
	OS      string `json:"os"`
	Online  bool   `json:"online"`
	Self    bool   `json:"self"`
}

type status struct {
	Self struct {
		HostName     string   `json:"HostName"`
		DNSName      string   `json:"DNSName"`
		OS           string   `json:"OS"`
		TailscaleIPs []string `json:"TailscaleIPs"`
		Online       bool     `json:"Online"`
	} `json:"Self"`
	Peer map[string]struct {
		HostName     string   `json:"HostName"`
		DNSName      string   `json:"DNSName"`
		OS           string   `json:"OS"`
		TailscaleIPs []string `json:"TailscaleIPs"`
		Online       bool     `json:"Online"`
	} `json:"Peer"`
}

// Status runs `tailscale status --json` and returns this node plus its peers.
//
// The CLI is used rather than the LocalAPI socket so that knot keeps working
// regardless of how tailscaled was started (systemd, Android, userspace mode).
func Status() ([]Device, error) {
	cmd := exec.Command("tailscale", "status", "--json")
	cmd.WaitDelay = 5 * time.Second

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}

	var st status
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("parse tailscale status: %w", err)
	}

	devices := []Device{{
		Name:    st.Self.HostName,
		DNSName: strings.TrimSuffix(st.Self.DNSName, "."),
		IP:      firstIP(st.Self.TailscaleIPs),
		OS:      st.Self.OS,
		Online:  true,
		Self:    true,
	}}

	for _, p := range st.Peer {
		devices = append(devices, Device{
			Name:    p.HostName,
			DNSName: strings.TrimSuffix(p.DNSName, "."),
			IP:      firstIP(p.TailscaleIPs),
			OS:      p.OS,
			Online:  p.Online,
		})
	}

	sort.Slice(devices, func(i, j int) bool {
		if devices[i].Self != devices[j].Self {
			return devices[i].Self
		}
		return devices[i].Name < devices[j].Name
	})
	return devices, nil
}

// firstIP returns the IPv4 tailnet address, which is what we dial.
func firstIP(ips []string) string {
	for _, ip := range ips {
		if !strings.Contains(ip, ":") {
			return ip
		}
	}
	if len(ips) > 0 {
		return ips[0]
	}
	return ""
}

// Resolve maps a node name to a dialable device. Matching prefers the MagicDNS
// first label, because a device's hostname is often stale or cloned (a laptop
// imaged from another can report the original's hostname) while the tailnet
// name is what the user actually configured.
func Resolve(devices []Device, name string) (Device, bool) {
	want := strings.ToLower(strings.TrimSpace(name))

	// Exact MagicDNS label, then full DNS name, then hostname.
	for _, match := range []func(Device) string{
		func(d Device) string { return dnsLabel(d.DNSName) },
		func(d Device) string { return d.DNSName },
		func(d Device) string { return d.Name },
	} {
		for _, d := range devices {
			if strings.ToLower(match(d)) == want {
				return d, true
			}
		}
	}
	return Device{}, false
}

// dnsLabel returns "thinkpad" for "thinkpad.tail895445.ts.net".
func dnsLabel(dnsName string) string {
	if i := strings.Index(dnsName, "."); i > 0 {
		return dnsName[:i]
	}
	return dnsName
}

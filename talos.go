package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// etcdMemberListResponse matches the JSON structure of `talosctl etcd member list -o json`.
type etcdMemberListResponse struct {
	Members []struct {
		Hostname string `json:"hostname"`
	} `json:"members"`
}

// getTalosconfig extracts the talosconfig from the cluster's secret and writes it
// to a temp file. Tries <cluster>-talosconfig and <cluster>-cp-talosconfig.
func (c *Controller) getTalosconfig(ctx context.Context, ns, clusterName string) (string, error) {
	secretNames := []string{
		clusterName + "-talosconfig",
		clusterName + "-cp-talosconfig",
	}

	for _, secretName := range secretNames {
		secret, err := c.kubeClient.CoreV1().Secrets(ns).Get(ctx, secretName, metav1.GetOptions{})
		if err != nil {
			continue
		}

		data, ok := secret.Data["talosconfig"]
		if !ok || len(data) == 0 {
			continue
		}

		tmpFile := filepath.Join(c.tmpDir, fmt.Sprintf("talosconfig-%s-%s", ns, clusterName))
		if err := os.WriteFile(tmpFile, data, 0600); err != nil {
			return "", fmt.Errorf("writing talosconfig to %s: %w", tmpFile, err)
		}

		return tmpFile, nil
	}

	return "", fmt.Errorf("talosconfig secret not found for cluster %s/%s", ns, clusterName)
}

// findHealthyEndpoints tries to reach each non-deleting machine via talosctl
// and returns all responding IP addresses. InternalIPs are tried first since
// the Talos API (especially etcd) may only be reachable on private networks.
func (c *Controller) findHealthyEndpoints(ctx context.Context, talosconfig string, machines []MachineInfo) ([]string, error) {
	// Collect candidate addresses: InternalIPs first, then ExternalIPs
	type candidate struct {
		address string
	}
	var candidates []candidate

	// First pass: collect all InternalIPs
	for _, m := range machines {
		if m.HasDeletionTS {
			continue
		}
		for _, addr := range m.Addresses {
			if addr.Type == "InternalIP" {
				candidates = append(candidates, candidate{address: addr.Address})
				break // one InternalIP per machine
			}
		}
	}
	// Second pass: collect ExternalIPs as fallback
	for _, m := range machines {
		if m.HasDeletionTS {
			continue
		}
		for _, addr := range m.Addresses {
			if addr.Type == "ExternalIP" {
				candidates = append(candidates, candidate{address: addr.Address})
				break // one ExternalIP per machine
			}
		}
	}

	var healthy []string
	for _, c2 := range candidates {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		cmd := exec.CommandContext(checkCtx, c.config.TalosctlPath,
			"--talosconfig", talosconfig,
			"-n", c2.address,
			"version", "--short")
		err := cmd.Run()
		cancel()

		if err == nil {
			healthy = append(healthy, c2.address)
		}
	}

	if len(healthy) == 0 {
		return nil, fmt.Errorf("no reachable machine found via Talos API")
	}
	return healthy, nil
}

// getEtcdMembers queries etcd member list from the given endpoint via talosctl
// and returns the list of member hostnames.
func (c *Controller) getEtcdMembers(ctx context.Context, talosconfig, endpoint string) ([]string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, c.config.TalosctlPath,
		"--talosconfig", talosconfig,
		"-n", endpoint,
		"etcd", "member", "list", "-o", "json")

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("talosctl etcd member list: %w", err)
	}

	var responses []etcdMemberListResponse
	if err := json.Unmarshal(output, &responses); err != nil {
		return nil, fmt.Errorf("parsing etcd member list response: %w", err)
	}

	var members []string
	for _, resp := range responses {
		for _, m := range resp.Members {
			if m.Hostname != "" {
				members = append(members, m.Hostname)
			}
		}
	}

	return members, nil
}

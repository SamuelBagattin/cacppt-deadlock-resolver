package main

import (
	"context"
	"fmt"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// getTalosClientConfig extracts the talosconfig from the cluster's secret and
// parses it into a client config. Tries <cluster>-talosconfig and <cluster>-cp-talosconfig.
func (c *Controller) getTalosClientConfig(ctx context.Context, ns, clusterName string) (*clientconfig.Config, error) {
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

		cfg, err := clientconfig.FromBytes(data)
		if err != nil {
			return nil, fmt.Errorf("parsing talosconfig from secret %s/%s: %w", ns, secretName, err)
		}

		return cfg, nil
	}

	return nil, fmt.Errorf("talosconfig secret not found for cluster %s/%s", ns, clusterName)
}

// collectEndpoints gathers candidate IP addresses from the machine list.
// InternalIPs are preferred, ExternalIPs are used as fallback.
// Machines with a deletion timestamp are skipped.
func collectEndpoints(machines []MachineInfo) []string {
	var endpoints []string

	// First pass: collect all InternalIPs
	for _, m := range machines {
		if m.HasDeletionTS {
			continue
		}
		for _, addr := range m.Addresses {
			if addr.Type == "InternalIP" {
				endpoints = append(endpoints, addr.Address)
				break
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
				endpoints = append(endpoints, addr.Address)
				break
			}
		}
	}

	return endpoints
}

// getEtcdMembers tries each endpoint in order via the Talos gRPC client
// and returns etcd member hostnames from the first endpoint that responds.
func (c *Controller) getEtcdMembers(ctx context.Context, cfg *clientconfig.Config, endpoints []string) ([]string, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no endpoints provided")
	}

	var lastErr error
	for _, ep := range endpoints {
		members, err := c.queryEtcdMembersFromEndpoint(ctx, cfg, ep)
		if err != nil {
			lastErr = err
			c.logger.Warn("etcd member list failed on endpoint, trying next", "endpoint", ep, "error", err)
			continue
		}
		return members, nil
	}

	return nil, fmt.Errorf("getting etcd members failed on all %d endpoints: %w", len(endpoints), lastErr)
}

func (c *Controller) queryEtcdMembersFromEndpoint(ctx context.Context, cfg *clientconfig.Config, endpoint string) ([]string, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	tc, err := talosclient.New(queryCtx, talosclient.WithEndpoints(endpoint), talosclient.WithConfig(cfg))
	if err != nil {
		return nil, fmt.Errorf("creating talos client for %s: %w", endpoint, err)
	}
	defer tc.Close() //nolint:errcheck

	nodeCtx := talosclient.WithNode(queryCtx, endpoint)

	resp, err := tc.EtcdMemberList(nodeCtx, &machineapi.EtcdMemberListRequest{QueryLocal: true})
	if err != nil {
		return nil, fmt.Errorf("etcd member list from %s: %w", endpoint, err)
	}

	var members []string
	for _, msg := range resp.Messages {
		for _, member := range msg.Members {
			if member.Hostname != "" {
				members = append(members, member.Hostname)
			}
		}
	}

	return members, nil
}

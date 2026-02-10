package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Controller watches TalosControlPlane resources and resolves etcd deadlocks
// caused by the non-atomic gracefulEtcdLeave/Machine delete in CACPPT.
type Controller struct {
	dynamicClient dynamic.Interface
	kubeClient    kubernetes.Interface
	config        Config
	states        map[string]*ClusterState
	mu            sync.Mutex
	logger        *slog.Logger
	tmpDir        string
}

// ClusterState tracks deadlock detection timing per cluster.
type ClusterState struct {
	DeadlockSince time.Time
	LastAction    time.Time
}

// MachineInfo holds data extracted from a CAPI Machine resource.
type MachineInfo struct {
	Name          string
	Namespace     string
	NodeName      string
	HasDeletionTS bool
	Addresses     []Address
}

// Address represents a machine network address.
type Address struct {
	Type    string
	Address string
}

var machineGVR = schema.GroupVersionResource{
	Group:    "cluster.x-k8s.io",
	Version:  "v1beta1",
	Resource: "machines",
}

// NewController creates a controller with Kubernetes clients.
// Uses in-cluster config by default, falls back to kubeconfig for local development.
func NewController(cfg Config, logger *slog.Logger) (*Controller, error) {
	restCfg, err := buildRestConfig(cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("building kubeconfig: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	kubeClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "cacppt-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}

	return &Controller{
		dynamicClient: dynClient,
		kubeClient:    kubeClient,
		config:        cfg,
		states:        make(map[string]*ClusterState),
		logger:        logger,
		tmpDir:        tmpDir,
	}, nil
}

func (c *Controller) tcpGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "controlplane.cluster.x-k8s.io",
		Version:  c.config.TCPAPIVersion,
		Resource: "taloscontrolplanes",
	}
}

// Run starts the main polling loop. Blocks until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) {
	defer os.RemoveAll(c.tmpDir)

	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()

	c.reconcileAll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.reconcileAll(ctx)
		}
	}
}

func (c *Controller) reconcileAll(ctx context.Context) {
	tcpList, err := c.dynamicClient.Resource(c.tcpGVR()).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Error("failed to list TalosControlPlane resources", "error", err)
		return
	}

	for i := range tcpList.Items {
		tcp := &tcpList.Items[i]
		if err := c.reconcileTCP(ctx, tcp); err != nil {
			c.logger.Error("reconciliation error",
				"namespace", tcp.GetNamespace(),
				"tcp", tcp.GetName(),
				"error", err)
		}
	}
}

func (c *Controller) reconcileTCP(ctx context.Context, tcp *unstructured.Unstructured) error {
	ns := tcp.GetNamespace()
	tcpName := tcp.GetName()
	log := c.logger.With("namespace", ns, "tcp", tcpName)

	// Extract cluster name from CAPI label, fall back to TCP name
	clusterName := tcpName
	if cn := tcp.GetLabels()["cluster.x-k8s.io/cluster-name"]; cn != "" {
		clusterName = cn
	}
	key := ns + "/" + clusterName

	// Desired replicas (default 3)
	desired, found, _ := unstructured.NestedInt64(tcp.Object, "spec", "replicas")
	if !found || desired == 0 {
		desired = 3
	}

	// List control plane machines for this cluster
	machines, err := c.listMachines(ctx, ns, clusterName)
	if err != nil {
		return fmt.Errorf("listing machines: %w", err)
	}
	machineCount := int64(len(machines))

	// Check EtcdClusterHealthy condition
	etcdStatus := getConditionStatus(tcp.Object, "EtcdClusterHealthy")

	// Happy path: etcd healthy or no rolling update in progress
	if etcdStatus == "True" || machineCount <= desired {
		c.mu.Lock()
		if state, ok := c.states[key]; ok && !state.DeadlockSince.IsZero() {
			log.Info("deadlock resolved, rollout progressing normally")
		}
		c.states[key] = &ClusterState{}
		c.mu.Unlock()
		return nil
	}

	// Potential deadlock: machines > desired AND etcd unhealthy
	now := time.Now()

	c.mu.Lock()
	state, ok := c.states[key]
	if !ok {
		state = &ClusterState{}
		c.states[key] = state
	}

	// First detection: record timestamp and wait
	if state.DeadlockSince.IsZero() {
		state.DeadlockSince = now
		c.mu.Unlock()
		log.Warn("potential deadlock detected",
			"machines", machineCount, "desired", desired, "etcdStatus", etcdStatus)
		return nil
	}

	elapsed := now.Sub(state.DeadlockSince)

	// Still within grace period
	if elapsed < c.config.StuckThreshold {
		c.mu.Unlock()
		return nil
	}

	// Rate limit: one action per threshold period per cluster
	if !state.LastAction.IsZero() && now.Sub(state.LastAction) < c.config.StuckThreshold {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	log.Info("deadlock confirmed",
		"elapsed", elapsed.Round(time.Second),
		"machines", machineCount, "desired", desired)

	// Get talosconfig from cluster secret
	talosconfig, err := c.getTalosconfig(ctx, ns, clusterName)
	if err != nil {
		log.Warn("cannot get talosconfig, skipping", "error", err)
		return nil
	}

	// Find reachable control plane nodes
	healthyIPs, err := c.findHealthyEndpoints(ctx, talosconfig, machines)
	if err != nil {
		log.Warn("cannot reach any machine via Talos API, skipping", "error", err)
		return nil
	}

	// Try each healthy endpoint for etcd member list until one succeeds
	var etcdMembers []string
	var lastErr error
	for _, ip := range healthyIPs {
		log.Info("querying etcd members", "endpoint", ip)
		etcdMembers, lastErr = c.getEtcdMembers(ctx, talosconfig, ip)
		if lastErr == nil {
			break
		}
		log.Warn("etcd member list failed on endpoint, trying next", "endpoint", ip, "error", lastErr)
	}
	if lastErr != nil && etcdMembers == nil {
		return fmt.Errorf("getting etcd members failed on all %d endpoints: %w", len(healthyIPs), lastErr)
	}
	log.Info("etcd member list", "members", etcdMembers, "count", len(etcdMembers))

	// Build member set for fast lookup
	memberSet := make(map[string]bool, len(etcdMembers))
	for _, m := range etcdMembers {
		memberSet[m] = true
	}

	// Find the stuck machine: not in etcd, no DeletionTimestamp, has a nodeRef
	var stuck *MachineInfo
	for i := range machines {
		m := &machines[i]
		if m.HasDeletionTS || m.NodeName == "" {
			continue
		}
		if !memberSet[m.NodeName] {
			stuck = m
			break
		}
	}

	if stuck == nil {
		log.Warn("all machines are etcd members, not the expected deadlock pattern, resetting")
		c.mu.Lock()
		c.states[key] = &ClusterState{}
		c.mu.Unlock()
		return nil
	}

	log.Info("found stuck machine", "machine", stuck.Name, "node", stuck.NodeName)

	if c.config.DryRun {
		log.Info("[DRY RUN] would delete machine", "machine", stuck.Name, "namespace", ns)
		return nil
	}

	// Delete the stuck machine to unblock the rollout
	log.Info("deleting stuck machine to unblock rollout", "machine", stuck.Name)
	err = c.dynamicClient.Resource(machineGVR).Namespace(ns).Delete(ctx, stuck.Name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("deleting machine %s: %w", stuck.Name, err)
	}
	log.Info("machine deleted, CACPPT should resume rollout", "machine", stuck.Name)

	c.mu.Lock()
	c.states[key] = &ClusterState{LastAction: now}
	c.mu.Unlock()

	return nil
}

func (c *Controller) listMachines(ctx context.Context, ns, clusterName string) ([]MachineInfo, error) {
	selector := fmt.Sprintf("cluster.x-k8s.io/cluster-name=%s,cluster.x-k8s.io/control-plane", clusterName)
	machineList, err := c.dynamicClient.Resource(machineGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, err
	}

	machines := make([]MachineInfo, 0, len(machineList.Items))
	for i := range machineList.Items {
		machines = append(machines, extractMachineInfo(&machineList.Items[i]))
	}
	return machines, nil
}

func getConditionStatus(obj map[string]interface{}, condType string) string {
	conditions, found, _ := unstructured.NestedSlice(obj, "status", "conditions")
	if !found {
		return "Unknown"
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := cond["type"].(string); t == condType {
			if s, _ := cond["status"].(string); s != "" {
				return s
			}
		}
	}
	return "Unknown"
}

func extractMachineInfo(machine *unstructured.Unstructured) MachineInfo {
	info := MachineInfo{
		Name:          machine.GetName(),
		Namespace:     machine.GetNamespace(),
		HasDeletionTS: machine.GetDeletionTimestamp() != nil,
	}

	info.NodeName, _, _ = unstructured.NestedString(machine.Object, "status", "nodeRef", "name")

	addresses, found, _ := unstructured.NestedSlice(machine.Object, "status", "addresses")
	if found {
		for _, a := range addresses {
			addr, ok := a.(map[string]interface{})
			if !ok {
				continue
			}
			addrType, _ := addr["type"].(string)
			addrValue, _ := addr["address"].(string)
			if addrType != "" && addrValue != "" {
				info.Addresses = append(info.Addresses, Address{Type: addrType, Address: addrValue})
			}
		}
	}

	return info
}

func buildRestConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	// Try in-cluster first
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	// Fall back to default kubeconfig for local development
	return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
}

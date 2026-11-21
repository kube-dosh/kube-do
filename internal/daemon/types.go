package daemon

import (
	"time"

	"github.com/kube-dosh/kube-do/internal/cluster"
)

// Node is the API representation of a cluster node.
type Node struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// NodeView adds a node's role to the API representation.
type NodeView struct {
	Node
	Role string `json:"role"`
}

// ClusterSummary is returned when listing clusters.
type ClusterSummary struct {
	Name         string     `json:"name"`
	Build        string     `json:"build"`
	ControlPlane []NodeView `json:"controlPlane"`
	Workers      []NodeView `json:"workers"`
	NodeCount    int        `json:"nodeCount"`
}

// ClusterDetail includes the complete stored ClusterConfig.
type ClusterDetail struct {
	Name         string     `json:"name"`
	Build        string     `json:"build"`
	Config       any        `json:"config"`
	ControlPlane []NodeView `json:"controlPlane"`
	Workers      []NodeView `json:"workers"`
}

// InstallRequest controls the asynchronous install job.
type InstallRequest struct {
	// Prepare downloads binaries, ensures the Registry and mirrors images
	// before rendering and running Ansible. It defaults to true.
	Prepare *bool `json:"prepare,omitempty"`
	// Check runs the preflight checks before Ansible. It defaults to true.
	Check     *bool    `json:"check,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	ExtraVars []string `json:"extraVars,omitempty"`
}

// AddNodesRequest asks the daemon to join workers to an existing cluster.
type AddNodesRequest struct {
	Role  string `json:"role,omitempty"`
	Nodes []Node `json:"nodes"`
}

// UpgradeRequest asks the daemon to perform an in-place upgrade to another
// Build. Prepare and Check default to true.
type UpgradeRequest struct {
	TargetBuild string   `json:"targetBuild"`
	Prepare     *bool    `json:"prepare,omitempty"`
	Check       *bool    `json:"check,omitempty"`
	ExtraVars   []string `json:"extraVars,omitempty"`
}

// RollbackRequest restores the cluster to a previous Build using an etcd
// snapshot captured before the upgrade.
type RollbackRequest struct {
	TargetBuild    string   `json:"targetBuild"`
	SnapshotPath   string   `json:"snapshotPath"`
	SnapshotSHA256 string   `json:"snapshotSha256,omitempty"`
	SnapshotSize   int64    `json:"snapshotSize,omitempty"`
	Prepare        *bool    `json:"prepare,omitempty"`
	Confirm        bool     `json:"confirm"`
	ExtraVars      []string `json:"extraVars,omitempty"`
}

// EtcdRestoreRequest restores an etcd snapshot. Confirm must be true.
type EtcdRestoreRequest struct {
	SnapshotPath string   `json:"snapshotPath"`
	TargetNode   string   `json:"targetNode,omitempty"`
	Confirm      bool     `json:"confirm"`
	ExtraVars    []string `json:"extraVars,omitempty"`
}

// UninstallRequest tears down Kubernetes state on every node. KeepCluster
// retains the stored ClusterConfig so the same definition can be reinstalled.
type UninstallRequest struct {
	Confirm     bool     `json:"confirm"`
	KeepCluster bool     `json:"keepCluster,omitempty"`
	ExtraVars   []string `json:"extraVars,omitempty"`
}

type JobStatus string

const (
	JobQueued      JobStatus = "queued"
	JobRunning     JobStatus = "running"
	JobSucceeded   JobStatus = "succeeded"
	JobFailed      JobStatus = "failed"
	JobCancelled   JobStatus = "cancelled"
	JobInterrupted JobStatus = "interrupted"
)

// Job describes an asynchronous cluster operation.
type Job struct {
	ID           string     `json:"id"`
	Kind         string     `json:"kind"`
	Cluster      string     `json:"cluster"`
	Status       JobStatus  `json:"status"`
	CreatedAt    time.Time  `json:"createdAt"`
	StartedAt    *time.Time `json:"startedAt,omitempty"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
	Error        string     `json:"error,omitempty"`
	NewWorkers   []Node     `json:"newWorkers,omitempty"`
	Nodes        []Node     `json:"nodes,omitempty"`
	LogBytes     int        `json:"logBytes"`
	LogTruncated bool       `json:"logTruncated,omitempty"`
	Result       any        `json:"result,omitempty"`
}

func toNode(n cluster.Node) Node {
	return Node{Name: n.Name, Address: n.Address}
}

func toNodes(nodes []cluster.Node) []Node {
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, toNode(n))
	}
	return out
}

func toNodeViews(nodes []cluster.Node, role string) []NodeView {
	out := make([]NodeView, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, NodeView{Node: toNode(n), Role: role})
	}
	return out
}

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

type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
)

// Job describes an asynchronous cluster operation.
type Job struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Cluster    string     `json:"cluster"`
	Status     JobStatus  `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Error      string     `json:"error,omitempty"`
	NewWorkers []Node     `json:"newWorkers,omitempty"`
	LogBytes   int        `json:"logBytes"`
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

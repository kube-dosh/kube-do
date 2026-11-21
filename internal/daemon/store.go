package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kube-dosh/kube-do/internal/atomicfile"
	"github.com/kube-dosh/kube-do/internal/cluster"
)

var ErrClusterNotFound = errors.New("cluster not found")

type Store struct {
	dir string
}

func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("data directory is required")
	}
	if err := os.MkdirAll(filepath.Join(dir, "clusters"), 0o755); err != nil {
		return nil, fmt.Errorf("create cluster store: %w", err)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) Save(cfg *cluster.Config) error {
	if cfg == nil {
		return errors.New("cluster config is nil")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	path, err := s.clusterPath(cfg.Metadata.Name)
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode cluster config: %w", err)
	}
	if err := atomicfile.Write(path, data, 0o600); err != nil {
		return fmt.Errorf("save cluster config: %w", err)
	}
	return nil
}

func (s *Store) Load(name string) (*cluster.Config, error) {
	path, err := s.clusterPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrClusterNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read cluster config: %w", err)
	}
	cfg, err := cluster.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse stored cluster config: %w", err)
	}
	return cfg, nil
}

func (s *Store) Exists(name string) (bool, error) {
	path, err := s.clusterPath(name)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, err
	}
}

func (s *Store) List() ([]*cluster.Config, error) {
	root := filepath.Join(s.dir, "clusters")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	var out []*cluster.Config
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		cfg, err := s.Load(entry.Name())
		if errors.Is(err, ErrClusterNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Metadata.Name < out[j].Metadata.Name
	})
	return out, nil
}

// Delete removes the stored ClusterConfig. It does not touch cluster nodes.
func (s *Store) Delete(name string) error {
	path, err := s.clusterPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete cluster config: %w", err)
	}
	return nil
}

func (s *Store) clusterPath(name string) (string, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return "", fmt.Errorf("invalid cluster name %q", name)
	}
	return filepath.Join(s.dir, "clusters", name, "cluster.yaml"), nil
}

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallRendersAndInvokesAnsible(t *testing.T) {
	root := t.TempDir()
	ansibleDir := filepath.Join(root, "ansible")
	if err := os.MkdirAll(ansibleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	playbook := filepath.Join(ansibleDir, "site.yml")
	if err := os.WriteFile(playbook, []byte("---\n- hosts: all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "cluster.yaml")
	if err := os.WriteFile(configPath, []byte(`apiVersion: kube-do.io/v1alpha1
kind: ClusterConfig
metadata:
  name: test
build: v1.36.5-kube-do.1
installer:
  address: 10.0.0.10
registry:
  address: 10.0.0.10:5000
controlPlane:
  vip: 10.0.0.100
  nodes:
    - name: cp-1
      address: 10.0.0.11
cni:
  name: cilium
  kubeProxyReplacement: true
`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(root, "ansible-args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "ansible-playbook"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	rendered := filepath.Join(root, "rendered")
	cmd := NewRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"install",
		"--config", configPath,
		"--ansible-dir", ansibleDir,
		"--out", rendered,
		"--limit", "cp-1",
		"--tags", "ha,init",
		"--extra-var", "ansible_user=root",
		"--check",
		"-v",
		"-v",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("install: %v", err)
	}
	for _, path := range []string{
		filepath.Join(rendered, "inventory.ini"),
		filepath.Join(rendered, "group_vars", "all.yml"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("rendered file %s: %v", path, err)
		}
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := string(args)
	for _, want := range []string{
		"-i\n" + filepath.Join(rendered, "inventory.ini") + "\n",
		"--extra-vars\nansible_user=root\n",
		"--limit\ncp-1\n",
		"--tags\nha,init\n",
		"--check\n",
		"-vv\n",
		playbook + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ansible args missing %q:\n%s", want, got)
		}
	}
}

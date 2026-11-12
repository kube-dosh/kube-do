package install

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestArgs(t *testing.T) {
	dir := t.TempDir()
	playbook := filepath.Join(dir, "site.yml")
	if err := os.WriteFile(playbook, []byte("---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args, err := Args(Options{
		AnsibleDir: dir,
		Playbook:   "site.yml",
		Inventory:  "inventory.ini",
		ExtraVars:  []string{"a=1"},
		Limit:      "cp-1",
		Tags:       []string{"containerd", "kubelet"},
		Check:      true,
		Verbosity:  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-i", "inventory.ini", "--extra-vars", "a=1", "--limit", "cp-1", "--tags", "containerd,kubelet", "--check", "-vv", playbook}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v\nwant %#v", args, want)
	}
}

func TestRunUsesRunner(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "site.yml"), []byte("---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "inventory.ini"), []byte("[all]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotName string
	var gotArgs []string
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), Options{
		AnsibleDir: dir,
		Playbook:   "site.yml",
		Inventory:  "inventory.ini",
		WorkingDir: dir,
		Stdout:     &stdout,
		Stderr:     &stderr,
		LookPath:   func(string) (string, error) { return "/usr/bin/ansible-playbook", nil },
		Runner: func(_ context.Context, name string, args []string, _, _ io.Writer) error {
			gotName = name
			gotArgs = args
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotName != "/usr/bin/ansible-playbook" {
		t.Fatalf("name = %q", gotName)
	}
	wantArgs := []string{"-i", "inventory.ini", filepath.Join(dir, "site.yml")}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestRunRequiresAnsiblePlaybook(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "site.yml"), []byte("---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inventory := filepath.Join(dir, "inventory.ini")
	if err := os.WriteFile(inventory, []byte("[all]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), Options{
		AnsibleDir: dir,
		Playbook:   "site.yml",
		Inventory:  inventory,
		LookPath: func(string) (string, error) {
			return "", errors.New("not found")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "ansible-playbook not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunRejectsMissingInventory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "site.yml"), []byte("---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), Options{
		AnsibleDir: dir,
		Playbook:   "site.yml",
		Inventory:  filepath.Join(dir, "missing.ini"),
		LookPath: func(string) (string, error) {
			return "/usr/bin/ansible-playbook", nil
		},
		Runner: func(context.Context, string, []string, io.Writer, io.Writer) error {
			t.Fatal("ansible runner must not be invoked with a missing inventory")
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "inventory") {
		t.Fatalf("err = %v", err)
	}
}

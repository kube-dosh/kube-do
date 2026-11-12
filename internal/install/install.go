// Package install drives ansible-playbook.
package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Options struct {
	AnsibleDir string
	Playbook   string
	Inventory  string
	WorkingDir string
	ExtraVars  []string
	Limit      string
	Tags       []string
	Check      bool
	Verbosity  int
	Stdout     io.Writer
	Stderr     io.Writer
	LookPath   func(string) (string, error)
	Runner     func(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error
}

// Args builds the ansible-playbook argument list.
func Args(opts Options) ([]string, error) {
	if opts.Inventory == "" {
		return nil, errors.New("inventory is required")
	}
	if opts.Playbook == "" {
		return nil, errors.New("playbook is required")
	}
	playbook := opts.Playbook
	if !filepath.IsAbs(playbook) && opts.AnsibleDir != "" {
		playbook = filepath.Join(opts.AnsibleDir, playbook)
	}
	if _, err := os.Stat(playbook); err != nil {
		return nil, fmt.Errorf("playbook %s: %w", playbook, err)
	}
	args := []string{"-i", opts.Inventory}
	for _, v := range opts.ExtraVars {
		args = append(args, "--extra-vars", v)
	}
	if opts.Limit != "" {
		args = append(args, "--limit", opts.Limit)
	}
	if len(opts.Tags) > 0 {
		args = append(args, "--tags", strings.Join(opts.Tags, ","))
	}
	if opts.Check {
		args = append(args, "--check")
	}
	if opts.Verbosity > 0 {
		args = append(args, "-"+strings.Repeat("v", opts.Verbosity))
	}
	args = append(args, playbook)
	return args, nil
}

// Run executes ansible-playbook.
func Run(ctx context.Context, opts Options) error {
	args, err := Args(opts)
	if err != nil {
		return err
	}
	inventory := opts.Inventory
	if !filepath.IsAbs(inventory) && opts.WorkingDir != "" {
		inventory = filepath.Join(opts.WorkingDir, inventory)
	}
	info, err := os.Stat(inventory)
	if err != nil {
		return fmt.Errorf("inventory %s: %w", inventory, err)
	}
	if info.IsDir() {
		return fmt.Errorf("inventory %s is a directory", inventory)
	}
	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	path, err := lookPath("ansible-playbook")
	if err != nil {
		return errors.New("ansible-playbook not found in PATH; install ansible-core on the Installer Host")
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	if opts.Runner != nil {
		return opts.Runner(ctx, path, args, stdout, stderr)
	}
	return defaultRunner(ctx, opts.WorkingDir, path, args, stdout, stderr)
}

func defaultRunner(ctx context.Context, workingDir, name string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = workingDir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

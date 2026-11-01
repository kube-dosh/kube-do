package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// Probe checks that a Registry answers over HTTPS at address.
func Probe(ctx context.Context, address, caFile string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid address: %w", err)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return errors.New("CA file contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+address+"/v2/", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized {
		return nil
	}
	return fmt.Errorf("unexpected status %s", resp.Status)
}

func portInUse(address string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

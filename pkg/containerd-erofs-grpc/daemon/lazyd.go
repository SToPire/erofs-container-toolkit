package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const defaultLazydStartTimeout = 10 * time.Second

type LazyDaemonConfig struct {
	Binary string
	Socket string
}

type LazyDaemon struct {
	config LazyDaemonConfig
	client *http.Client

	mu  sync.Mutex
	cmd *exec.Cmd
}

type lazydBlobDescriptor struct {
	Digest    string `json:"digest"`
	Size      uint64 `json:"size"`
	MediaType string `json:"media_type,omitempty"`
}

type lazydRemoteSource struct {
	Type     string `json:"type"`
	ImageRef string `json:"image_ref"`
}

type lazydAuthConfig struct {
	Username string `json:"username"`
	Secret   string `json:"secret"`
}

type lazydInstanceConfig struct {
	TargetPath string              `json:"target_path"`
	Blob       lazydBlobDescriptor `json:"blob"`
	Source     lazydRemoteSource   `json:"source"`
	Auth       *lazydAuthConfig    `json:"auth,omitempty"`
}

func NewLazyDaemon(cfg LazyDaemonConfig) (*LazyDaemon, error) {
	if cfg.Binary == "" {
		return nil, fmt.Errorf("lazyd binary is required")
	}
	if cfg.Socket == "" {
		return nil, fmt.Errorf("lazyd socket is required")
	}
	return &LazyDaemon{
		config: cfg,
		client: newLazydHTTPClient(cfg.Socket),
	}, nil
}

func newLazydHTTPClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}}
}

func (d *LazyDaemon) Start(ctx context.Context, _ DaemonConfig) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.cmd != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(d.config.Socket), 0o700); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, d.config.Binary, "--socket", d.config.Socket)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start lazyd: %w", err)
	}
	d.cmd = cmd

	if err := d.waitReady(ctx); err != nil {
		_ = d.stopLocked(context.Background())
		return err
	}
	return nil
}

func (d *LazyDaemon) Stop(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopLocked(ctx)
}

func (d *LazyDaemon) stopLocked(ctx context.Context) error {
	if d.cmd == nil || d.cmd.Process == nil {
		return nil
	}
	cmd := d.cmd
	d.cmd = nil

	done := make(chan error, 1)
	go func() {
		_ = cmd.Process.Kill()
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if _, ok := err.(*exec.ExitError); ok {
			return nil
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *LazyDaemon) BindLayer(ctx context.Context, instanceID string, cfg RemoteLayerConfig, targetPath string) error {
	if instanceID == "" {
		return fmt.Errorf("instance id is required")
	}
	if targetPath == "" {
		return fmt.Errorf("target path is required")
	}
	if cfg.ImageRef == "" {
		return fmt.Errorf("image ref is required")
	}
	if cfg.Layer.Size < 0 {
		return fmt.Errorf("layer size must be non-negative")
	}
	if err := createSparseTarget(targetPath, cfg.Layer); err != nil {
		return err
	}

	request := lazydInstanceConfig{
		TargetPath: targetPath,
		Blob: lazydBlobDescriptor{
			Digest:    cfg.Layer.Digest.String(),
			Size:      uint64(cfg.Layer.Size),
			MediaType: cfg.Layer.MediaType,
		},
		Source: lazydRemoteSource{
			Type:     "oci-registry",
			ImageRef: cfg.ImageRef,
		},
	}
	if cfg.Username != "" || cfg.Secret != "" {
		request.Auth = &lazydAuthConfig{Username: cfg.Username, Secret: cfg.Secret}
	}

	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return d.do(ctx, http.MethodPut, "/api/v1/instances/"+instanceID, body)
}

func (d *LazyDaemon) UnbindLayer(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return fmt.Errorf("instance id is required")
	}
	return d.do(ctx, http.MethodDelete, "/api/v1/instances/"+instanceID, nil)
}

func (d *LazyDaemon) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, defaultLazydStartTimeout)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		if err := d.do(ctx, http.MethodGet, "/api/v1/daemon", nil); err == nil {
			return nil
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for lazyd: %w: last error: %v", ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func (d *LazyDaemon) do(ctx context.Context, method, path string, body []byte) error {
	if d.client == nil {
		d.client = newLazydHTTPClient(d.config.Socket)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("lazyd %s %s failed: status=%d body=%s", method, path, resp.StatusCode, string(data))
	}
	return nil
}

func createSparseTarget(targetPath string, layer ocispec.Descriptor) error {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(layer.Size)
}

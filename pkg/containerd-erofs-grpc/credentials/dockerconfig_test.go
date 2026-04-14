package credentials

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDockerConfigBackendNormalizesDockerIO(t *testing.T) {
	dir := t.TempDir()
	writeDockerConfig(t, dir, map[string]any{
		"auths": map[string]any{
			DefaultDockerRegistryAuthHost: map[string]any{
				"auth": base64.StdEncoding.EncodeToString([]byte("user:pass")),
			},
		},
	})

	backend := NewDockerConfigBackend(dir)
	username, secret, err := backend.Lookup(context.Background(), "docker.io")
	if err != nil {
		t.Fatalf("resolve docker.io credentials: %v", err)
	}
	if username != "user" || secret != "pass" {
		t.Fatalf("unexpected docker.io credentials %q/%q", username, secret)
	}
}

func TestDockerConfigBackendUsesCredHelpers(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	writeHelperScript(t, filepath.Join(binDir, "docker-credential-testhelper"), `#!/bin/sh
if [ "$1" = "get" ]; then
  read host
  if [ "$host" = "registry.example.com" ]; then
    printf '{"ServerURL":"%s","Username":"helper-user","Secret":"helper-pass"}\n' "$host"
    exit 0
  fi
  printf 'credentials not found in native keychain\n'
  exit 1
fi
printf 'unsupported\n'
exit 1
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	writeDockerConfig(t, dir, map[string]any{
		"credHelpers": map[string]any{
			"registry.example.com": "testhelper",
		},
	})

	backend := NewDockerConfigBackend(dir)
	username, secret, err := backend.Lookup(context.Background(), "registry.example.com")
	if err != nil {
		t.Fatalf("resolve helper credentials: %v", err)
	}
	if username != "helper-user" || secret != "helper-pass" {
		t.Fatalf("unexpected helper credentials %q/%q", username, secret)
	}
}

func TestDockerConfigBackendUsesCredsStore(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	writeHelperScript(t, filepath.Join(binDir, "docker-credential-testhelper"), `#!/bin/sh
if [ "$1" = "get" ]; then
  read host
  printf '{"ServerURL":"%s","Username":"store-user","Secret":"store-pass"}\n' "$host"
  exit 0
fi
printf 'unsupported\n'
exit 1
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	writeDockerConfig(t, dir, map[string]any{
		"credsStore": "testhelper",
	})

	backend := NewDockerConfigBackend(dir)
	username, secret, err := backend.Lookup(context.Background(), "another.registry.example")
	if err != nil {
		t.Fatalf("resolve credsStore credentials: %v", err)
	}
	if username != "store-user" || secret != "store-pass" {
		t.Fatalf("unexpected credsStore credentials %q/%q", username, secret)
	}
}

func writeDockerConfig(t *testing.T, dir string, cfg map[string]any) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal docker config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, DockerConfigFileName), data, 0o644); err != nil {
		t.Fatalf("write docker config: %v", err)
	}
}

func writeHelperScript(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
}

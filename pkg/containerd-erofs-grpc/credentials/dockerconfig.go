package credentials

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	DockerConfigDirEnv            = "DOCKER_CONFIG"
	DockerConfigFileName          = "config.json"
	DockerCredentialHelperPrefix  = "docker-credential-"
	DefaultDockerRegistryAuthHost = "https://index.docker.io/v1/"
)

type DockerConfigBackend struct {
	configPath string
}

type dockerConfigFile struct {
	AuthConfigs       map[string]dockerAuthConfig `json:"auths"`
	CredentialsStore  string                      `json:"credsStore,omitempty"`
	CredentialHelpers map[string]string           `json:"credHelpers,omitempty"`
}

type dockerAuthConfig struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	Auth          string `json:"auth,omitempty"`
	ServerAddress string `json:"serveraddress,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
	RegistryToken string `json:"registrytoken,omitempty"`
}

type helperCredentials struct {
	ServerURL string `json:"ServerURL"`
	Username  string `json:"Username"`
	Secret    string `json:"Secret"`
}

func NewDockerConfigBackend(configPath string) *DockerConfigBackend {
	return &DockerConfigBackend{configPath: configPath}
}

func (b *DockerConfigBackend) Lookup(_ context.Context, host string) (string, string, error) {
	cfg, err := b.loadConfig()
	if err != nil {
		return "", "", err
	}

	lookupHost := normalizeCredentialHost(host)
	helper := cfg.helperForHost(lookupHost)
	if helper != "" {
		auth, err := lookupHelperAuth(helper, lookupHost)
		if err != nil {
			return "", "", fmt.Errorf("resolve credential helper for host %q: %w", host, err)
		}
		switch {
		case auth.IdentityToken != "":
			return "", auth.IdentityToken, nil
		case auth.RegistryToken != "":
			return "", auth.RegistryToken, nil
		default:
			return auth.Username, auth.Password, nil
		}
	}

	auth := cfg.fileAuth(lookupHost)
	switch {
	case auth.IdentityToken != "":
		return "", auth.IdentityToken, nil
	case auth.RegistryToken != "":
		return "", auth.RegistryToken, nil
	default:
		return auth.Username, auth.Password, nil
	}
}

func (b *DockerConfigBackend) loadConfig() (*dockerConfigFile, error) {
	path, err := b.resolveConfigPath()
	if err != nil {
		return nil, err
	}

	cfg := &dockerConfigFile{
		AuthConfigs: map[string]dockerAuthConfig{},
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read docker config %q: %w", path, err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("decode docker config %q: %w", path, err)
	}
	if cfg.AuthConfigs == nil {
		cfg.AuthConfigs = map[string]dockerAuthConfig{}
	}

	for registry, auth := range cfg.AuthConfigs {
		if auth.Auth != "" {
			username, password, err := decodeDockerAuth(auth.Auth)
			if err != nil {
				return nil, err
			}
			auth.Username = username
			auth.Password = password
			auth.Auth = ""
		}
		auth.ServerAddress = registry
		cfg.AuthConfigs[registry] = auth
	}

	return cfg, nil
}

func (b *DockerConfigBackend) resolveConfigPath() (string, error) {
	if b.configPath != "" {
		info, err := os.Stat(b.configPath)
		if err != nil {
			return "", fmt.Errorf("stat docker config path %q: %w", b.configPath, err)
		}
		if info.IsDir() {
			return filepath.Join(b.configPath, DockerConfigFileName), nil
		}
		if filepath.Base(b.configPath) != DockerConfigFileName {
			return "", fmt.Errorf("docker config path %q must be a directory or %s", b.configPath, DockerConfigFileName)
		}
		return b.configPath, nil
	}

	if dir := os.Getenv(DockerConfigDirEnv); dir != "" {
		return filepath.Join(dir, DockerConfigFileName), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(home, ".docker", DockerConfigFileName), nil
}

func (c *dockerConfigFile) helperForHost(host string) string {
	if c.CredentialHelpers != nil {
		if helper := c.CredentialHelpers[host]; helper != "" {
			return helper
		}
	}
	return c.CredentialsStore
}

func (c *dockerConfigFile) fileAuth(host string) dockerAuthConfig {
	if auth, ok := c.AuthConfigs[host]; ok {
		return auth
	}
	for registry, auth := range c.AuthConfigs {
		if host == convertToHostname(registry) {
			return auth
		}
	}
	return dockerAuthConfig{}
}

func lookupHelperAuth(helperSuffix, host string) (dockerAuthConfig, error) {
	cmd := exec.Command(DockerCredentialHelperPrefix+helperSuffix, "get")
	cmd.Stdin = strings.NewReader(host)
	output, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if msg == "credentials not found in native keychain" {
			return dockerAuthConfig{}, nil
		}
		return dockerAuthConfig{}, fmt.Errorf("helper %q failed: %w", helperSuffix, err)
	}

	var creds helperCredentials
	if err := json.Unmarshal(output, &creds); err != nil {
		return dockerAuthConfig{}, fmt.Errorf("decode helper response: %w", err)
	}

	auth := dockerAuthConfig{
		ServerAddress: host,
	}
	if creds.Username == "<token>" {
		auth.IdentityToken = creds.Secret
	} else {
		auth.Username = creds.Username
		auth.Password = creds.Secret
	}
	return auth, nil
}

func decodeDockerAuth(auth string) (string, string, error) {
	decoded, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		return "", "", fmt.Errorf("decode docker auth entry: %w", err)
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	if !ok || username == "" {
		return "", "", fmt.Errorf("invalid docker auth entry")
	}
	return username, strings.Trim(password, "\x00"), nil
}

func convertToHostname(value string) string {
	if strings.Contains(value, "://") {
		u, err := url.Parse(value)
		if err == nil && u.Hostname() != "" {
			if u.Port() == "" {
				return u.Hostname()
			}
			return net.JoinHostPort(u.Hostname(), u.Port())
		}
	}
	host, _, _ := strings.Cut(value, "/")
	return host
}

func normalizeCredentialHost(host string) string {
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io", DefaultDockerRegistryAuthHost:
		return DefaultDockerRegistryAuthHost
	default:
		return strings.TrimSuffix(host, "/")
	}
}

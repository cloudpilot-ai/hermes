package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type dockerConfigJSON struct {
	Auths map[string]dockerAuthConfig `json:"auths"`
}

type dockerAuthConfig struct {
	Username       string `json:"username"`
	Password       string `json:"password"`
	Auth           string `json:"auth"`
	IdentityToken  string `json:"identitytoken"`
	IdentityToken2 string `json:"identityToken"`
}

func registryAuthsFromPod(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod) ([]RegistryAuth, error) {
	if client == nil || pod == nil || len(pod.Spec.ImagePullSecrets) == 0 {
		return nil, nil
	}

	authsByHost := map[string]RegistryAuth{}
	var errs []string
	for _, ref := range pod.Spec.ImagePullSecrets {
		if ref.Name == "" {
			continue
		}
		secret, err := client.CoreV1().Secrets(pod.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", ref.Name, err))
			continue
		}
		auths, err := registryAuthsFromSecret(secret)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", ref.Name, err))
			continue
		}
		for _, auth := range auths {
			authsByHost[auth.Host] = auth
		}
	}

	auths := registryAuthList(authsByHost)
	if len(errs) > 0 {
		return auths, errors.New(strings.Join(errs, "; "))
	}
	return auths, nil
}

func registryAuthsFromSecret(secret *corev1.Secret) ([]RegistryAuth, error) {
	if secret == nil {
		return nil, fmt.Errorf("secret is nil")
	}
	if data, ok := secret.Data[corev1.DockerConfigJsonKey]; ok {
		var cfg dockerConfigJSON
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", corev1.DockerConfigJsonKey, err)
		}
		return registryAuthsFromDockerConfig(cfg.Auths), nil
	}
	if data, ok := secret.Data[corev1.DockerConfigKey]; ok {
		var cfg map[string]dockerAuthConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", corev1.DockerConfigKey, err)
		}
		return registryAuthsFromDockerConfig(cfg), nil
	}
	return nil, fmt.Errorf("missing %s or %s", corev1.DockerConfigJsonKey, corev1.DockerConfigKey)
}

func registryAuthsFromDockerConfig(items map[string]dockerAuthConfig) []RegistryAuth {
	authsByHost := map[string]RegistryAuth{}
	for server, item := range items {
		host := normalizeRegistryHost(server)
		if host == "" {
			continue
		}
		username, secret := dockerCredentials(item)
		if username == "" && secret == "" {
			continue
		}
		authsByHost[host] = RegistryAuth{
			Host:     host,
			Username: username,
			Secret:   secret,
		}
	}
	return registryAuthList(authsByHost)
}

func dockerCredentials(item dockerAuthConfig) (string, string) {
	username := item.Username
	secret := item.Password
	if secret == "" {
		secret = item.IdentityToken
	}
	if secret == "" {
		secret = item.IdentityToken2
	}
	if (username == "" || secret == "") && item.Auth != "" {
		if decoded, err := base64.StdEncoding.DecodeString(item.Auth); err == nil {
			parts := strings.SplitN(string(decoded), ":", 2)
			if len(parts) == 2 {
				if username == "" {
					username = parts[0]
				}
				if secret == "" {
					secret = parts[1]
				}
			}
		}
	}
	return username, secret
}

func registryResolver(auths []RegistryAuth) remotes.Resolver {
	if len(auths) == 0 {
		return nil
	}
	return docker.NewResolver(docker.ResolverOptions{
		Client: http.DefaultClient,
		Credentials: func(host string) (string, string, error) {
			for _, auth := range auths {
				if registryHostsMatch(auth.Host, host) {
					return auth.Username, auth.Secret, nil
				}
			}
			return "", "", nil
		},
	})
}

func registryHostsMatch(a, b string) bool {
	a = normalizeRegistryHost(a)
	b = normalizeRegistryHost(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	return isDockerHubHost(a) && isDockerHubHost(b)
}

func isDockerHubHost(host string) bool {
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return true
	default:
		return false
	}
}

func normalizeRegistryHost(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.Contains(value, "://") {
		if u, err := url.Parse(value); err == nil {
			value = u.Host
		}
	}
	value = strings.TrimPrefix(value, "//")
	if i := strings.IndexByte(value, '/'); i >= 0 {
		value = value[:i]
	}
	return strings.ToLower(value)
}

func registryAuthList(items map[string]RegistryAuth) []RegistryAuth {
	hosts := make([]string, 0, len(items))
	for host := range items {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)

	auths := make([]RegistryAuth, 0, len(hosts))
	for _, host := range hosts {
		auths = append(auths, items[host])
	}
	return auths
}

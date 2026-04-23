package k8s

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestParseImagePullPolicy(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    corev1.PullPolicy
		wantErr bool
	}{
		{"empty defaults to PullAlways", "", corev1.PullAlways, false},
		{"whitespace-only defaults to PullAlways", "   ", corev1.PullAlways, false},
		{"Always", "Always", corev1.PullAlways, false},
		{"IfNotPresent", "IfNotPresent", corev1.PullIfNotPresent, false},
		{"Never", "Never", corev1.PullNever, false},
		{"surrounding whitespace tolerated", "  IfNotPresent  ", corev1.PullIfNotPresent, false},
		{"invalid rejected", "Sometimes", "", true},
		{"lowercase rejected", "always", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseImagePullPolicy(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got policy=%q", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("parseImagePullPolicy(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParseImagePullSecrets(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty returns nil", "", nil},
		{"single entry", "ghcr-pull", []string{"ghcr-pull"}},
		{"multiple entries preserve order", "ghcr-pull,acr-pull", []string{"ghcr-pull", "acr-pull"}},
		{"whitespace around entries trimmed", "  ghcr-pull  ,  acr-pull  ", []string{"ghcr-pull", "acr-pull"}},
		{"empty entries dropped", "ghcr-pull,,acr-pull,", []string{"ghcr-pull", "acr-pull"}},
		{"whitespace-only returns nil", "   ", nil},
		{"only commas returns nil", ",,,", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseImagePullSecrets(tt.raw)
			if !stringSlicesEqual(got, tt.want) {
				t.Errorf("parseImagePullSecrets(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestBuildPodImagePullPolicy(t *testing.T) {
	cfg := runtime.Config{
		Command: "/bin/bash",
		Env:     map[string]string{"GC_AGENT": "test"},
	}

	tests := []struct {
		name   string
		policy corev1.PullPolicy
	}{
		{"Always", corev1.PullAlways},
		{"IfNotPresent", corev1.PullIfNotPresent},
		{"Never", corev1.PullNever},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newProviderWithOps(newFakeK8sOps())
			p.imagePullPolicy = tt.policy

			pod, err := buildPod("test-pod", cfg, p)
			if err != nil {
				t.Fatal(err)
			}
			if got := pod.Spec.Containers[0].ImagePullPolicy; got != tt.policy {
				t.Errorf("main container ImagePullPolicy = %q, want %q", got, tt.policy)
			}
		})
	}
}

func TestBuildPodImagePullPolicyDefault(t *testing.T) {
	// A provider constructed with empty env (i.e. newProviderWithOps)
	// must default the agent container ImagePullPolicy to PullAlways so
	// current behaviour is preserved.
	p := newProviderWithOps(newFakeK8sOps())
	cfg := runtime.Config{
		Command: "/bin/bash",
		Env:     map[string]string{"GC_AGENT": "test"},
	}
	pod, err := buildPod("test-pod", cfg, p)
	if err != nil {
		t.Fatal(err)
	}
	if got := pod.Spec.Containers[0].ImagePullPolicy; got != corev1.PullAlways {
		t.Errorf("default ImagePullPolicy = %q, want %q", got, corev1.PullAlways)
	}
}

func TestBuildPodInitContainerUsesConfiguredPullPolicy(t *testing.T) {
	// The staging init container runs the same image as the main container,
	// so a private-registry pull must use the same configured credentials
	// and policy. Without this, GC_K8S_IMAGE_PULL_POLICY=Never would still
	// try to pull for staging, and Always would silently reuse a stale image.
	cfg := runtime.Config{
		Command:    "/bin/bash",
		WorkDir:    "/city/demo-rig",
		OverlayDir: "/some/overlay", // triggers staging
		Env: map[string]string{
			"GC_AGENT": "demo-rig/polecat",
			"GC_CITY":  "/city",
		},
	}

	for _, policy := range []corev1.PullPolicy{corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever} {
		t.Run(string(policy), func(t *testing.T) {
			p := newProviderWithOps(newFakeK8sOps())
			p.imagePullPolicy = policy

			pod, err := buildPod("test-pod", cfg, p)
			if err != nil {
				t.Fatal(err)
			}
			if len(pod.Spec.InitContainers) == 0 {
				t.Fatal("expected staging init container")
			}
			if got := pod.Spec.InitContainers[0].ImagePullPolicy; got != policy {
				t.Errorf("init container ImagePullPolicy = %q, want %q", got, policy)
			}
		})
	}
}

func TestBuildPodImagePullSecrets(t *testing.T) {
	cfg := runtime.Config{
		Command: "/bin/bash",
		Env:     map[string]string{"GC_AGENT": "test"},
	}

	t.Run("none configured", func(t *testing.T) {
		p := newProviderWithOps(newFakeK8sOps())
		pod, err := buildPod("test-pod", cfg, p)
		if err != nil {
			t.Fatal(err)
		}
		if len(pod.Spec.ImagePullSecrets) != 0 {
			t.Errorf("ImagePullSecrets = %v, want none", pod.Spec.ImagePullSecrets)
		}
	})

	t.Run("single secret", func(t *testing.T) {
		p := newProviderWithOps(newFakeK8sOps())
		p.imagePullSecrets = []string{"ghcr-pull"}
		pod, err := buildPod("test-pod", cfg, p)
		if err != nil {
			t.Fatal(err)
		}
		got := pod.Spec.ImagePullSecrets
		if len(got) != 1 || got[0].Name != "ghcr-pull" {
			t.Errorf("ImagePullSecrets = %v, want [{Name:ghcr-pull}]", got)
		}
	})

	t.Run("multiple secrets preserve order", func(t *testing.T) {
		p := newProviderWithOps(newFakeK8sOps())
		p.imagePullSecrets = []string{"ghcr-pull", "acr-pull"}
		pod, err := buildPod("test-pod", cfg, p)
		if err != nil {
			t.Fatal(err)
		}
		got := pod.Spec.ImagePullSecrets
		want := []string{"ghcr-pull", "acr-pull"}
		if len(got) != len(want) {
			t.Fatalf("ImagePullSecrets length = %d, want %d (%v)", len(got), len(want), got)
		}
		for i, ref := range got {
			if ref.Name != want[i] {
				t.Errorf("ImagePullSecrets[%d].Name = %q, want %q", i, ref.Name, want[i])
			}
		}
	})
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

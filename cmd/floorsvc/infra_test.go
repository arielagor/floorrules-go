package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile("../../" + path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// M7 (day-2 review): the Terraform declared a security posture that
// nothing enforced. Binary Authorization was switched on with no policy
// (the default project policy admits every image), the control plane had a
// public endpoint and no authorized networks, cloudsql.client was granted
// but never used, and Secret Manager IAM went to a GSA while the pod read a
// plain Kubernetes Secret. These checks pin each control to a real
// resource; terraform validate and kubeconform check the syntax.
func TestTerraformControlsAreReal(t *testing.T) {
	tf := readRepoFile(t, "infra/terraform/main.tf")
	for _, want := range []string{
		`resource "google_binary_authorization_policy"`,
		`evaluation_mode         = "REQUIRE_ATTESTATION"`,
		`require_attestations_by`,
		`resource "google_binary_authorization_attestor"`,
		`master_authorized_networks_config`,
		`secret_manager_config`,
	} {
		if !strings.Contains(tf, want) {
			t.Errorf("main.tf: missing %s", want)
		}
	}
	if strings.Contains(tf, "roles/cloudsql.client") {
		t.Error("main.tf grants roles/cloudsql.client, which nothing uses (private IP + password DSN)")
	}
	// Secret access goes to the pod's own Kubernetes identity, which is what
	// the Secret Manager CSI add-on authenticates as.
	accessors := regexp.MustCompile(`(?s)resource "google_secret_manager_secret_iam_member".*?member\s*=\s*"([^"]+)"`).FindAllStringSubmatch(tf, -1)
	if len(accessors) == 0 {
		t.Fatal("no Secret Manager accessor bindings")
	}
	for _, a := range accessors {
		if !strings.HasPrefix(a[1], "principal://iam.googleapis.com/") {
			t.Errorf("secret accessor %q is not a Workload Identity Federation principal for a KSA", a[1])
		}
	}
}

func TestPodsReadSecretsThroughSecretManagerCSI(t *testing.T) {
	for _, f := range []string{"deploy/k8s/deployment.yaml", "deploy/k8s/migrate-job.yaml"} {
		y := readRepoFile(t, f)
		if !strings.Contains(y, "driver: secrets-store-gke.csi.k8s.io") {
			t.Errorf("%s: secrets volume does not use the Secret Manager CSI driver", f)
		}
		if regexp.MustCompile(`(?m)^\s+secret:\s*$`).MatchString(y) {
			t.Errorf("%s: still mounts a Kubernetes Secret", f)
		}
	}
	spc := readRepoFile(t, "deploy/k8s/secretproviderclass.yaml")
	if !strings.Contains(spc, "provider: gke") {
		t.Error("SecretProviderClass does not use the gke provider")
	}
}

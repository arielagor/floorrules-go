package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// manifestDoc returns the YAML document in file whose metadata.name is name.
func manifestDoc(t *testing.T, file, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/k8s/" + file)
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		if regexp.MustCompile(`(?m)^  name: ` + regexp.QuoteMeta(name) + `$`).MatchString(doc) {
			return doc
		}
	}
	t.Fatalf("%s: no document named %s", file, name)
	return ""
}

// M6 (day-2 review): ingress was admitted only from a `gateway` namespace,
// but a GKE Gateway (gke-l7-global-external-managed) reaches pods through
// NEGs from Google's proxy and health-check ranges, so every request and
// probe would have been dropped. Nothing admitted a metrics scraper either.
func TestNetworkPolicyAdmitsLoadBalancerAndScraper(t *testing.T) {
	np := manifestDoc(t, "networkpolicy.yaml", "floorsvc-allow")
	for _, want := range []string{
		"cidr: 35.191.0.0/16",                         // health checks and GFE proxies
		"cidr: 130.211.0.0/22",                        // GFE proxies
		"kubernetes.io/metadata.name: gke-gmp-system", // managed Prometheus collectors on Autopilot
		"port: 9090",
	} {
		if !strings.Contains(np, want) {
			t.Errorf("floorsvc-allow does not admit %q", want)
		}
	}
	if strings.Contains(np, "kubernetes.io/metadata.name: gateway") {
		t.Error("floorsvc-allow still assumes an in-cluster gateway namespace")
	}
	dep := manifestDoc(t, "deployment.yaml", "floorsvc")
	if !regexp.MustCompile(`name: metrics\s+containerPort: 9090`).MatchString(dep) {
		t.Error("deployment does not expose a metrics port 9090")
	}
	pm := manifestDoc(t, "podmonitoring.yaml", "floorsvc")
	if !strings.Contains(pm, "kind: PodMonitoring") || !strings.Contains(pm, "port: metrics") {
		t.Error("no PodMonitoring scraping the metrics port")
	}
}

// The Deployment's grace period must cover the whole shutdown sequence in
// run(): readiness drain, then the longest possible apply (execution plus
// recording). Kubernetes sends SIGKILL when it runs out, which is exactly
// the mid-apply exit H2 was about.
func TestDeploymentGraceCoversLongestApply(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/k8s/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	y := string(raw)
	grace := regexp.MustCompile(`terminationGracePeriodSeconds:\s*(\d+)`).FindStringSubmatch(y)
	if grace == nil {
		t.Fatal("terminationGracePeriodSeconds not set")
	}
	secs, _ := strconv.Atoi(grace[1])

	env := func(name string) time.Duration {
		m := regexp.MustCompile(`- name: ` + name + `\s+value:\s*"?([0-9a-z]+)"?`).FindStringSubmatch(y)
		if m == nil {
			t.Fatalf("%s not set in deployment.yaml", name)
		}
		d, err := time.ParseDuration(m[1])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return d
	}
	need := env("SHUTDOWN_DRAIN") + max(env("SHUTDOWN_WAIT"), env("APPLY_TIMEOUT")+env("RECORD_TIMEOUT")) + 5*time.Second
	if time.Duration(secs)*time.Second < need {
		t.Fatalf("terminationGracePeriodSeconds=%d, need at least %s", secs, need)
	}
}

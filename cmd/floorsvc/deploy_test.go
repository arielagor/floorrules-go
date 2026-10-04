package main

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"
)

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

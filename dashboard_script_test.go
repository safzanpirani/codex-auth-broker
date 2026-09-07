package main

import (
	"os/exec"
	"testing"
)

func TestDashboardJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is needed for dashboard behavior tests; the broker has no Node runtime dependency")
	}
	if output, err := exec.Command(node, "--test", "scripts/dashboard.test.cjs").CombinedOutput(); err != nil {
		t.Fatalf("dashboard behavior tests failed: %v\n%s", err, output)
	}
}

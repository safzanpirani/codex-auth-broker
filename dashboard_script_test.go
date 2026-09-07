package main

import (
	"os/exec"
	"testing"
)

func TestDashboardJavaScript(t *testing.T) {
	runBrowserScriptTest(t, "scripts/dashboard.test.cjs")
}

func TestVoiceJavaScript(t *testing.T) {
	runBrowserScriptTest(t, "scripts/voice.test.cjs")
}

func runBrowserScriptTest(t *testing.T, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is needed for dashboard behavior tests; the broker has no Node runtime dependency")
	}
	if output, err := exec.Command(node, "--test", script).CombinedOutput(); err != nil {
		t.Fatalf("browser behavior tests failed: %v\n%s", err, output)
	}
}

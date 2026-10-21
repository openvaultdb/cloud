package main

import (
	"gopkg.in/yaml.v3"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type deploymentStep struct {
	Name string
	Run  string
	Env  map[string]string
}

func deploymentSteps(t *testing.T) []deploymentStep {
	t.Helper()
	var workflow struct {
		Jobs map[string]struct{ Steps []deploymentStep }
	}
	if err := yaml.Unmarshal([]byte(readServerFile(t, "..", ".github", "workflows", "deploy-chinook-cloudrun.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow.Jobs["deploy"].Steps
}
func TestCloudRunExactSHAGuardExecutesAuthoritativeLookup(t *testing.T) {
	var script string
	for _, step := range deploymentSteps(t) {
		if step.Name == "Reject stale main" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("missing authoritative guard")
	}
	for _, tc := range []struct {
		name, sha, status string
		ok                bool
	}{
		{"exact", strings.Repeat("a", 40), "0", true}, {"stale", strings.Repeat("b", 40), "0", false}, {"failed-query-with-correct-output", strings.Repeat("a", 40), "8", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nprintf '%s\trefs/heads/main\n' \"$LOOKUP_SHA\"\nexit \"$LOOKUP_STATUS\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-eu", "-o", "pipefail", "-c", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "DEPLOY_SHA="+strings.Repeat("a", 40), "LOOKUP_SHA="+tc.sha, "LOOKUP_STATUS="+tc.status)
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.ok {
				t.Fatalf("guard=%v: %s", err, out)
			}
		})
	}
}

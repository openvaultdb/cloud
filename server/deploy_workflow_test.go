package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type deploymentStep struct {
	Name string
	ID   string `yaml:"id"`
	If   string `yaml:"if"`
	Run  string
	Env  map[string]string
	With map[string]string
}

func deploymentSteps(t *testing.T, file string) []deploymentStep {
	t.Helper()
	var workflow struct {
		Jobs map[string]struct {
			If    string
			Steps []deploymentStep
		}
	}
	if err := yaml.Unmarshal([]byte(readServerFile(t, "..", ".github", "workflows", file)), &workflow); err != nil {
		t.Fatal(err)
	}
	job := workflow.Jobs["deploy"]
	if job.If != "${{ github.event.workflow_run.conclusion == 'success' && github.event.workflow_run.head_branch == 'main' }}" {
		t.Fatal("deployment job's successful-main condition changed")
	}
	return job.Steps
}

func deploymentStepNamed(t *testing.T, steps []deploymentStep, name string) (int, deploymentStep) {
	t.Helper()
	for i, step := range steps {
		if step.Name == name {
			return i, step
		}
	}
	t.Fatalf("deployment step missing: %s", name)
	return 0, deploymentStep{}
}

// Execute the actual workflow scripts with authored CLI seams. No cloud command,
// HTTP request, provider body or listener is used by this harness.
const deploymentCLISeams = `
gcloud() {
  if [ "${1:-} ${2:-}" = 'run deploy' ]; then
    printf 'deploy\n' >> "$SYNTHETIC_CALLS"
  fi
  printf '%s\n' "$SYNTHETIC_URL"
}
git() {
  printf '%s\trefs/heads/main\n' "$SYNTHETIC_MAIN"
  return "$SYNTHETIC_GIT_EXIT"
}
curl() { printf 'curl\n' >> "$SYNTHETIC_CALLS"; }
jq() { :; }
grep() { :; }
python3() {
  if [ "${1:-}" = smoke_inventory.py ]; then
    printf 'inventory\n' >> "$SYNTHETIC_CALLS"
  else
    command "$SYNTHETIC_PYTHON" "$@"
  fi
}
`

func runDeploymentScript(t *testing.T, dir, script, origin, mainSHA string) error {
	return runDeploymentScriptWithGit(t, dir, script, origin, mainSHA, "0", true)
}

func runDeploymentScriptWithGit(t *testing.T, dir, script, origin, mainSHA, gitExit string, pipefail bool) error {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--noprofile", "--norc", "-e"}
	if pipefail {
		args = append(args, "-o", "pipefail")
	}
	cmd := exec.Command("bash", append(args, "-c", deploymentCLISeams+script)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"SYNTHETIC_URL="+origin, "SYNTHETIC_MAIN="+mainSHA,
		"SYNTHETIC_GIT_EXIT="+gitExit,
		"SYNTHETIC_PYTHON="+python, "SYNTHETIC_CALLS="+filepath.Join(dir, "calls"),
		"DEPLOY_SHA="+strings.Repeat("a", 40), "SERVICE_URL="+origin,
		"SERVICE_NAME=synthetic", "PROJECT_ID=synthetic", "REGION=synthetic",
		"RUNNER_TEMP="+dir, "GITHUB_OUTPUT="+filepath.Join(dir, "outputs"))
	output, err := cmd.CombinedOutput()
	if len(output) > 0 {
		t.Logf("authored workflow script output: %s", output)
	}
	return err
}

func TestCloudRunExactSHAGuardsRequireSuccessfulGitLookup(t *testing.T) {
	upstream := deploymentSteps(t, "deploy-chinook-cloudrun.yml")
	downstream := deploymentSteps(t, "deploy.yml")
	sha, origin := strings.Repeat("a", 40), "https://synthetic.a.run.app"
	for _, guard := range []struct {
		steps []deploymentStep
		name  string
	}{
		{upstream, "Reject an older main commit"},
		{upstream, "Deploy the public read-only service"},
		{upstream, "Record deployed service origin and source"},
		{upstream, "Verify live profile and cacheable DTQL"},
		{downstream, "Reject stale deployment"},
	} {
		_, step := deploymentStepNamed(t, guard.steps, guard.name)
		for _, mode := range []struct {
			name     string
			pipefail bool
		}{{"bash-e", false}, {"bash-eo-pipefail", true}} {
			for _, lookup := range []struct {
				name, output, status string
				wantSuccess          bool
			}{
				{"current", sha, "0", true},
				{"stale", strings.Repeat("b", 40), "0", false},
				{"correct-output-exit8", sha, "8", false},
			} {
				t.Run(guard.name+"/"+mode.name+"/"+lookup.name, func(t *testing.T) {
					dir := t.TempDir()
					err := runDeploymentScriptWithGit(t, dir, step.Run, origin, lookup.output, lookup.status, mode.pipefail)
					if (err == nil) != lookup.wantSuccess {
						t.Fatalf("git lookup success=%v: step error=%v", lookup.wantSuccess, err)
					}
					if !lookup.wantSuccess {
						for _, file := range []string{"chinook-run-origin.txt", "chinook-deploy-sha.txt", "outputs"} {
							if _, err := os.Stat(filepath.Join(dir, file)); !os.IsNotExist(err) {
								t.Fatal("failed lookup published metadata", file, err)
							}
						}
					}
					if guard.name == "Deploy the public read-only service" {
						calls, err := os.ReadFile(filepath.Join(dir, "calls"))
						if lookup.wantSuccess {
							if err != nil || string(calls) != "deploy\n" {
								t.Fatal("successful lookup did not reach synthetic deployment", string(calls), err)
							}
						} else if !os.IsNotExist(err) {
							t.Fatal("failed lookup reached deployment", string(calls), err)
						}
					}
				})
			}
		}
	}
}

func TestCloudRunSmokeIsOptInAndDeploymentReceiptSurvives(t *testing.T) {
	steps := deploymentSteps(t, "deploy-chinook-cloudrun.yml")
	deployIndex, deploy := deploymentStepNamed(t, steps, "Deploy the public read-only service")
	receiptIndex, receipt := deploymentStepNamed(t, steps, "Record deployed service origin and source")
	smokeIndex, smoke := deploymentStepNamed(t, steps, "Verify live profile and cacheable DTQL")
	uploadIndex, upload := deploymentStepNamed(t, steps, "Publish deployment metadata for the Worker deploy")
	if deployIndex >= receiptIndex || receiptIndex >= smokeIndex || smokeIndex >= uploadIndex ||
		deploy.If != "" || receipt.If != "" || upload.If != "" || receipt.ID != "deployment" ||
		!strings.Contains(deploy.Run, "gcloud run deploy") {
		t.Fatal("deployment or its unconditional receipt was gated or reordered")
	}
	if smoke.If != "${{ vars.OVDB_CLOUD_RUN_SMOKE_ENABLED == 'true' }}" ||
		smoke.Env["SERVICE_URL"] != "${{ steps.deployment.outputs.origin }}" {
		t.Fatal("live smoke requires an explicit repository opt-in and validated origin")
	}
	if upload.With["name"] != "chinook-cloudrun-deployment" || upload.With["if-no-files-found"] != "error" ||
		strings.Join(strings.Fields(upload.With["path"]), ",") != "server/chinook-run-origin.txt,server/chinook-deploy-sha.txt" {
		t.Fatal("downstream deployment artifact contract changed")
	}
	// Every live query remains in the gated step, including the inventory script.
	for i, step := range steps {
		if (strings.Contains(step.Run, "curl ") || strings.Contains(step.Run, "smoke_inventory.py")) && i != smokeIndex {
			t.Fatal("live request escaped the opt-in smoke step")
		}
	}
	_, downstream := deploymentStepNamed(t, deploymentSteps(t, "deploy.yml"), "Read deployment receipt")
	_, download := deploymentStepNamed(t, deploymentSteps(t, "deploy.yml"), "Download verified Cloud Run origin")
	if download.With["name"] != upload.With["name"] {
		t.Fatal("Worker downloads a different deployment artifact")
	}
	sha, origin := strings.Repeat("a", 40), "https://synthetic-service.a.run.app"
	for _, variable := range []string{"", "false", "true"} {
		t.Run("variable="+variable, func(t *testing.T) {
			dir := t.TempDir()
			if err := runDeploymentScript(t, dir, receipt.Run, origin, sha); err != nil {
				t.Fatal("metadata-only deployment receipt failed", err)
			}
			// The expression above is a literal variable equality; unset/false skip.
			if variable == "true" {
				if err := runDeploymentScript(t, dir, smoke.Run, origin, sha); err != nil {
					t.Fatal("authored enabled smoke control failed", err)
				}
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if variable == "true" {
				if err != nil || strings.Count(string(calls), "curl\n") != 6 || strings.Count(string(calls), "inventory\n") != 1 {
					t.Fatal("enabled smoke positive control did not execute every synthetic request", string(calls), err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("default-off deployment invoked smoke", string(calls), err)
			}
			artifact := filepath.Join(dir, "chinook-deployment")
			if err := os.Mkdir(artifact, 0700); err != nil {
				t.Fatal(err)
			}
			for file, want := range map[string]string{"chinook-run-origin.txt": origin, "chinook-deploy-sha.txt": sha} {
				data, err := os.ReadFile(filepath.Join(dir, file))
				if err != nil || string(data) != want+"\n" {
					t.Fatal("deployment metadata missing or changed", file, err)
				}
				if err := os.WriteFile(filepath.Join(artifact, file), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := runDeploymentScript(t, dir, downstream.Run, origin, sha); err != nil {
				t.Fatal("actual downstream receipt script refused metadata", err)
			}
			outputs, err := os.ReadFile(filepath.Join(dir, "outputs"))
			if err != nil || !strings.Contains(string(outputs), "sha="+sha+"\n") || !strings.Contains(string(outputs), "origin="+origin+"\n") {
				t.Fatal("Worker release inputs missing", err)
			}
		})
	}
}

func TestCloudRunDeploymentReceiptRefusesUntrustedOriginAndStaleSHA(t *testing.T) {
	_, receipt := deploymentStepNamed(t, deploymentSteps(t, "deploy-chinook-cloudrun.yml"), "Record deployed service origin and source")
	sha := strings.Repeat("a", 40)
	for _, origin := range []string{"", "http://synthetic.a.run.app", "https://example.invalid", "https://user@synthetic.a.run.app", "https://synthetic.a.run.app/", "https://synthetic.a.run.app?q=1", "https://synthetic.a.run.app#fragment", "https://synthetic.a.run.app:443"} {
		t.Run(origin, func(t *testing.T) {
			dir := t.TempDir()
			if err := runDeploymentScript(t, dir, receipt.Run, origin, sha); err == nil {
				t.Fatal("untrusted origin produced deployment metadata")
			}
			if _, err := os.Stat(filepath.Join(dir, "chinook-run-origin.txt")); !os.IsNotExist(err) {
				t.Fatal("invalid origin published a receipt", err)
			}
		})
	}
	dir := t.TempDir()
	if err := runDeploymentScript(t, dir, receipt.Run, "https://synthetic.a.run.app", strings.Repeat("b", 40)); err == nil {
		t.Fatal("stale deployed SHA produced metadata")
	}
	if _, err := os.Stat(filepath.Join(dir, "chinook-deploy-sha.txt")); !os.IsNotExist(err) {
		t.Fatal("stale SHA published a receipt", err)
	}
}

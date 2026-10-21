package main

import (
	"encoding/json"
	"fmt"
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

// Execute the actual workflow scripts with deterministic CLI seams. Subprocess
// gcloud and HTTP helpers have no credentials, network requests, or listeners.
const deploymentGcloudSeam = `#!/usr/bin/env python3
import json,os,sys
from pathlib import Path
args=sys.argv[1:]
state=Path("state.json")
mode=state.read_text() if state.exists() else "prior"
with open("calls","a") as calls: calls.write(" ".join(args[:3])+"\n")
image="registry.example/server@sha256:"
if args[:4] == ["artifacts","docker","images","describe"]:
    print("sha256:"+"a"*64)
elif args[:2] == ["run","deploy"]:
    if os.environ.get("SCENARIO") == "startup-failure": sys.exit(9)
    if not any(arg == "--image="+image+"a"*64 for arg in args): sys.exit(10)
    state.write_text("candidate")
elif args[:3] == ["run","services","update-traffic"]:
    if "--to-revisions=prior-a=60,prior-b=40" not in args: sys.exit(11)
    state.write_text("restored")
elif args[:3] == ["run","services","describe"]:
    traffic=[{"revisionName":"candidate","percent":100}] if mode == "candidate" else [{"revisionName":"prior-a","percent":60},{"revisionName":"prior-b","percent":40}]
    if mode == "candidate" and os.environ.get("SCENARIO") == "wrong-traffic": traffic=[{"revisionName":"prior-a","percent":100}]
    print(json.dumps({"status":{"url":os.environ["SYNTHETIC_URL"],"latestReadyRevisionName":"unused-latest-ready","traffic":traffic}, "spec":{"template":{"spec":{"containers":[{"image":"mutable:unused"}]}}}}))
elif args[:3] == ["run","revisions","describe"]:
    name=args[3]
    digest="a" if name == "candidate" else "b" if name == "prior-a" else "c"
    if name == "candidate" and os.environ.get("SCENARIO") == "wrong-image": digest="d"
    if mode == "restored" and os.environ.get("SCENARIO") == "rollback-verification-failure": digest="d"
    source="a" if name == "candidate" else "b"
    if name == "candidate" and os.environ.get("SCENARIO") == "wrong-source": source="d"
    print(json.dumps({"status":{"imageDigest":image+digest*64},"metadata":{"labels":{"source-sha":source*40}}}))
else:
    sys.exit(12)
`

const deploymentSmokeSeam = `import json,os,sys
from pathlib import Path
def require_status(origin,path,expected=200,**kwargs):
    with open("http-calls","a") as calls: calls.write(json.dumps({"origin":origin,"path":path,"expected":expected})+"\n")
    if not origin.startswith("https://"): raise RuntimeError("invalid independent prior origin")
    return {},b"{}"
if __name__ == "__main__":
    mode=Path("state.json").read_text()
    with open("http-calls","a") as calls: calls.write("six-provider-smoke-queries\n")
    if mode == "candidate" and os.environ.get("SCENARIO") == "smoke-failure": sys.exit(13)
    receipt=sys.argv[sys.argv.index("--receipt")+1]
    Path(receipt).write_text(json.dumps({"actualSeamExecuted":True}))
`

func deploymentHarness(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	gcloud := strings.Replace(deploymentGcloudSeam, "#!/usr/bin/env python3", "#!"+python, 1)
	for path, content := range map[string]string{filepath.Join(bin, "gcloud"): gcloud, filepath.Join(dir, "smoke_inventory.py"): deploymentSmokeSeam} {
		if err := os.WriteFile(path, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(dir, "fixture", "output")
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal(err)
	}
	inventory := `{"version":2,"databases":[{"id":"adventureworks"},{"id":"employees"},{"id":"chinook"},{"id":"northwind"},{"id":"pubs"},{"id":"sakila"},{"id":"shipping"},{"id":"shipping-logs"}]}`
	if err := os.WriteFile(filepath.Join(output, "inventory.json"), []byte(inventory), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runWorkflow(t *testing.T, dir, script, scenario, origin, mainSHA, gitExit string) error {
	t.Helper()
	seam := `git() { printf '%s\trefs/heads/main\n' "$SYNTHETIC_MAIN"; return "$SYNTHETIC_GIT_EXIT"; }
`
	cmd := exec.Command("bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", seam+script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"), "SCENARIO="+scenario, "SYNTHETIC_URL="+origin, "SYNTHETIC_MAIN="+mainSHA, "SYNTHETIC_GIT_EXIT="+gitExit, "DEPLOY_SHA="+strings.Repeat("a", 40), "IMAGE=registry.example/server:"+strings.Repeat("a", 40), "SERVICE_NAME=synthetic", "PROJECT_ID=synthetic", "REGION=synthetic", "RUNNER_TEMP="+dir, "GITHUB_OUTPUT="+filepath.Join(dir, "outputs"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("authored workflow seam: %s", output)
	}
	return err
}
func runStep(t *testing.T, dir string, step deploymentStep, scenario string) error {
	return runWorkflow(t, dir, step.Run, scenario, "https://synthetic.a.run.app", strings.Repeat("a", 40), "0")
}
func receiptJSON(t *testing.T, dir, file string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err = json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestCloudRunExactSHAGuardsRequireSuccessfulGitLookup(t *testing.T) {
	upstream := deploymentSteps(t, "deploy-chinook-cloudrun.yml")
	downstream := deploymentSteps(t, "deploy.yml")
	for _, guard := range []struct {
		steps []deploymentStep
		name  string
	}{
		{upstream, "Reject an older main commit"}, {upstream, "Build and push the validated image"}, {upstream, "Deploy the public read-only service"}, {upstream, "Record deployed service origin and source"}, {upstream, "Verify live profile and cacheable DTQL"}, {downstream, "Reject stale deployment"},
	} {
		_, step := deploymentStepNamed(t, guard.steps, guard.name)
		for _, lookup := range []struct{ name, sha, status string }{{"stale", strings.Repeat("b", 40), "0"}, {"correct-output-exit8", strings.Repeat("a", 40), "8"}} {
			t.Run(guard.name+"/"+lookup.name, func(t *testing.T) {
				dir := deploymentHarness(t)
				if err := runWorkflow(t, dir, step.Run, "", "https://synthetic.a.run.app", lookup.sha, lookup.status); err == nil {
					t.Fatal("failed authoritative main query accepted")
				}
				for _, file := range []string{"chinook-run-origin.txt", "chinook-deploy-sha.txt", "outputs", "state.json"} {
					if _, err := os.Stat(filepath.Join(dir, file)); !os.IsNotExist(err) {
						t.Fatal("failed guard mutated or published", file)
					}
				}
			})
		}
	}
}

func TestCloudRunPublicationRequiresLiveAcceptanceAndServingDigest(t *testing.T) {
	steps := deploymentSteps(t, "deploy-chinook-cloudrun.yml")
	_, capture := deploymentStepNamed(t, steps, "Capture rollback target")
	_, deploy := deploymentStepNamed(t, steps, "Deploy the public read-only service")
	_, record := deploymentStepNamed(t, steps, "Record deployed service origin and source")
	_, live := deploymentStepNamed(t, steps, "Verify live profile and cacheable DTQL")
	_, upload := deploymentStepNamed(t, steps, "Publish deployment metadata for the Worker deploy")
	if live.If != "" || !strings.Contains(live.Run, "--publication-receipt") || !strings.Contains(deploy.Run, `--image="${CANDIDATE_IMAGE}"`) || !strings.Contains(upload.With["path"], "shipping-publication-rollback-receipt.json") {
		t.Fatal("publication acceptance/digest/retained artifact contract missing")
	}
	for _, scenario := range []string{"valid", "wrong-image", "wrong-source", "wrong-traffic"} {
		t.Run(scenario, func(t *testing.T) {
			dir := deploymentHarness(t)
			if err := runStep(t, dir, capture, scenario); err != nil {
				t.Fatal(err)
			}
			prior := receiptJSON(t, dir, "shipping-publication-deploy-receipt.json")
			revisions := prior["priorRevisions"].(map[string]any)
			if len(revisions) != 2 || revisions["unused-latest-ready"] != nil || prior["serviceOrigin"] != "https://synthetic.a.run.app" {
				t.Fatal("prior serving traffic/digests not captured independently", prior)
			}
			if err := runStep(t, dir, deploy, scenario); err != nil {
				t.Fatal(err)
			}
			err := runStep(t, dir, record, scenario)
			if (err == nil) != (scenario == "valid") {
				t.Fatal("serving identity counterexample verdict", err)
			}
			if scenario != "valid" {
				if _, err := os.Stat(filepath.Join(dir, "chinook-deploy-sha.txt")); !os.IsNotExist(err) {
					t.Fatal("wrong serving identity published")
				}
				return
			}
			if err := runStep(t, dir, live, scenario); err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{"chinook-run-origin.txt", "chinook-deploy-sha.txt", "shipping-publication-live-receipt.json"} {
				if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
					t.Fatal(err)
				}
			}
			_, downstream := deploymentStepNamed(t, deploymentSteps(t, "deploy.yml"), "Read deployment receipt")
			artifact := filepath.Join(dir, "chinook-deployment")
			if err := os.Mkdir(artifact, 0700); err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{"chinook-run-origin.txt", "chinook-deploy-sha.txt"} {
				data, err := os.ReadFile(filepath.Join(dir, file))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(artifact, file), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := runStep(t, dir, downstream, scenario); err != nil {
				t.Fatal("downstream metadata compatibility", err)
			}
		})
	}
}

func TestCloudRunStartupAndSmokeFailureRollbackExecuteAndRetainEvidence(t *testing.T) {
	steps := deploymentSteps(t, "deploy-chinook-cloudrun.yml")
	_, capture := deploymentStepNamed(t, steps, "Capture rollback target")
	_, deploy := deploymentStepNamed(t, steps, "Deploy the public read-only service")
	_, record := deploymentStepNamed(t, steps, "Record deployed service origin and source")
	_, live := deploymentStepNamed(t, steps, "Verify live profile and cacheable DTQL")
	_, rollback := deploymentStepNamed(t, steps, "Roll back a failed publication")
	for _, scenario := range []string{"startup-failure", "smoke-failure", "rollback-verification-failure"} {
		t.Run(scenario, func(t *testing.T) {
			dir := deploymentHarness(t)
			if err := runStep(t, dir, capture, scenario); err != nil {
				t.Fatal(err)
			}
			deployErr := runStep(t, dir, deploy, scenario)
			if scenario == "startup-failure" {
				if deployErr == nil {
					t.Fatal("startup counterexample succeeded")
				}
			} else {
				if deployErr != nil {
					t.Fatal(deployErr)
				}
				if err := runStep(t, dir, record, scenario); err != nil {
					t.Fatal(err)
				}
				if scenario == "smoke-failure" {
					if err := runStep(t, dir, live, scenario); err == nil {
						t.Fatal("smoke counterexample succeeded")
					}
				}
			}
			err := runStep(t, dir, rollback, scenario)
			if (err == nil) != (scenario != "rollback-verification-failure") {
				t.Fatal("rollback expected verification outcome", err)
			}
			receipt := receiptJSON(t, dir, "shipping-publication-rollback-receipt.json")
			if receipt["verified"] != (scenario != "rollback-verification-failure") || receipt["failedImage"] != "registry.example/server@sha256:"+strings.Repeat("a", 64) || receipt["completedAt"] == nil || receipt["restoredRevisions"] == nil {
				t.Fatal("rollback evidence missing", receipt)
			}
			checks := receipt["checks"].([]any)
			if len(checks) != 6 {
				t.Fatal("rollback did not attempt every verification", receipt)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "http-calls"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(calls), "six-provider-smoke-queries") || !strings.Contains(string(calls), `"expected": 403`) || strings.Count(string(calls), `"expected": 404`) != 2 {
				t.Fatal("rollback omitted queries/absence/prior-origin checks", string(calls))
			}
			if scenario == "startup-failure" && receipt["failedRevision"] != nil {
				t.Fatal("startup failure invented a deployed revision")
			}
		})
	}
}

func TestCloudRunDeploymentReceiptRefusesUntrustedOrigin(t *testing.T) {
	steps := deploymentSteps(t, "deploy-chinook-cloudrun.yml")
	_, capture := deploymentStepNamed(t, steps, "Capture rollback target")
	for _, origin := range []string{"", "http://synthetic.a.run.app", "https://example.invalid", "https://user@synthetic.a.run.app", "https://synthetic.a.run.app/", "https://synthetic.a.run.app?q=1", "https://synthetic.a.run.app#fragment", "https://synthetic.a.run.app:443"} {
		t.Run(fmt.Sprintf("%q", origin), func(t *testing.T) {
			dir := deploymentHarness(t)
			if err := runWorkflow(t, dir, capture.Run, "", origin, strings.Repeat("a", 40), "0"); err == nil {
				t.Fatal("invalid prior origin accepted")
			}
			if _, err := os.Stat(filepath.Join(dir, "shipping-publication-deploy-receipt.json")); !os.IsNotExist(err) {
				t.Fatal("invalid rollback origin published")
			}
		})
	}
}

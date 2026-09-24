package infraci_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestBuildWorkflow(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(".github", "workflows", "build.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On struct {
			Dispatch struct{ Inputs map[string]any } `yaml:"workflow_dispatch"`
		}
		Concurrency struct {
			Group  string
			Cancel bool `yaml:"cancel-in-progress"`
			Queue  string
		}
		Jobs map[string]struct {
			Needs       any
			Name        string
			RunsOn      string `yaml:"runs-on"`
			Outputs     map[string]string
			Permissions map[string]string
			Strategy    struct{ Matrix string }
			Steps       []struct {
				ID, If, Shell string
				Uses, Run     string
				With, Env     map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}

	t.Run("builder swap", func(t *testing.T) {
		for _, step := range workflow.Jobs["admit"].Steps {
			if step.ID == "setup-swap" {
				t.Fatal("admission must skip swap setup")
			}
		}
		for _, name := range []string{"build", "builder"} {
			steps := workflow.Jobs[name].Steps
			if len(steps) == 0 || steps[0].ID != "setup-swap" {
				t.Fatalf("%s must configure swap before checkout", name)
			}
			step := steps[0]
			if step.If != "runner.os == 'Linux'" {
				t.Fatalf("%s swap setup must only run on Linux", name)
			}
			if step.Shell != "bash" || step.Run == "" {
				t.Fatalf("%s swap setup requires a Bash script", name)
			}
			cmd := exec.Command("bash", "-n")
			cmd.Stdin = strings.NewReader(step.Run)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s swap setup syntax: %s: %v", name, output, err)
			}
		}
	})

	t.Run("publication", func(t *testing.T) {
		if workflow.Concurrency.Group == "" || workflow.Concurrency.Cancel || workflow.Concurrency.Queue != "max" {
			t.Fatal("builds must queue under the publication lock")
		}
		if len(workflow.Jobs) != 3 {
			t.Fatal("cache publication belongs to the platform coordinators")
		}
	})

	t.Run("admission", func(t *testing.T) {
		for _, input := range []string{"source", "request", "host", "package"} {
			if workflow.On.Dispatch.Inputs[input] == nil {
				t.Fatalf("missing dispatch input: %s", input)
			}
		}
		for _, output := range []string{"matrix", "helpers", "revision"} {
			if workflow.Jobs["admit"].Outputs[output] != "${{ steps.process.outputs."+output+" }}" {
				t.Fatalf("missing admission output: %s", output)
			}
		}
		for name, output := range map[string]string{"build": "matrix", "builder": "helpers"} {
			job := workflow.Jobs[name]
			if job.Needs != "admit" || job.Strategy.Matrix != "${{ fromJSON(needs.admit.outputs."+output+") }}" || job.RunsOn != "${{ matrix.runner }}" {
				t.Fatalf("%s must use the admitted runners without waiting for other build jobs", name)
			}
		}
		// ActionJobs identifies coordinator results by this name.
		if workflow.Jobs["build"].Name != "Build ${{ matrix.system }}" {
			t.Fatal("build job name does not identify its system")
		}
		if workflow.Jobs["builder"].Permissions["packages"] != "write" {
			t.Fatal("helpers need to upload encrypted build results")
		}
	})

	actionPin := regexp.MustCompile(`^[^@]+@[a-f0-9]{40}$`)
	goPin := regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	nixPin := regexp.MustCompile(`^https://releases\.nixos\.org/nix/nix-\d+\.\d+\.\d+/install$`)
	goVersion, nixVersion := "", ""
	for _, name := range []string{"admit", "build", "builder"} {
		t.Run(name, func(t *testing.T) {
			job, ok := workflow.Jobs[name]
			if !ok {
				t.Fatal("missing worker job")
			}
			ref := "${{ needs.admit.outputs.revision }}"
			if name == "admit" {
				ref = "${{ inputs.source }}"
			}
			inputs := map[string]string{
				"INPUT_SOURCE":      "${{ inputs.source }}",
				"INPUT_HOST":        "${{ inputs.host }}",
				"INPUT_PACKAGE":     "${{ inputs.package }}",
				"INPUT_REQUEST":     "${{ inputs.request }}",
				"INPUT_SOURCE_PATH": "${{ github.workspace }}/source",
				"INPUT_MODE":        name,
			}
			if name == "builder" {
				inputs["INPUT_BUILDER"] = "${{ matrix.builder }}"
			}
			if name == "build" {
				inputs["NIX_SIGNING_KEY"] = "${{ secrets.NIX_SIGNING_KEY }}"
			}
			goReady, nixReady, process := false, false, false
			checkouts := 0
			for _, step := range job.Steps {
				if step.Uses != "" && !actionPin.MatchString(step.Uses) {
					t.Fatalf("action is not pinned: %s", step.Uses)
				}
				switch {
				case strings.HasPrefix(step.Uses, "actions/checkout@"):
					if step.With["repository"] != "${{ secrets.CI_SOURCE_REPOSITORY }}" || step.With["ssh-key"] != "${{ secrets.CI_DEPLOY_KEY }}" || step.With["persist-credentials"] != "false" {
						t.Fatal("checkout must remove private source credentials")
					}
					if step.With["path"] != "source" || step.With["ref"] != ref {
						t.Fatal("checkout does not use the admitted source")
					}
					checkouts++
				case strings.HasPrefix(step.Uses, "actions/setup-go@"):
					version := step.With["go-version"]
					if !goPin.MatchString(version) || step.With["cache"] != "false" || (goVersion != "" && goVersion != version) {
						t.Fatal("workers require the same pinned Go version with public caching disabled")
					}
					goVersion, goReady = version, true
				case strings.HasPrefix(step.Uses, "cachix/install-nix-action@"):
					version := step.With["install_url"]
					if !nixPin.MatchString(version) || (nixVersion != "" && nixVersion != version) {
						t.Fatal("workers require the same pinned Nix installer")
					}
					for _, setting := range []string{"sandbox = true", "accept-flake-config = false"} {
						if !strings.Contains(step.With["extra_nix_config"], setting) {
							t.Fatalf("missing worker Nix setting: %s", setting)
						}
					}
					nixVersion, nixReady = version, true
				}
				if name != "build" && step.Env["NIX_SIGNING_KEY"] != "" {
					t.Fatal("only coordinators may receive the cache signing key")
				}
				if step.Env["INPUT_MODE"] == "" {
					continue
				}
				if !goReady || !nixReady || checkouts != 1 {
					t.Fatal("worker requires checkout and toolchains before execution")
				}
				if !strings.HasPrefix(step.Uses, "actions/github-script@") || step.With["script"] == "" || step.Run != "" {
					t.Fatal("worker requires a JavaScript action for job-scoped cache credentials")
				}
				for key, want := range inputs {
					if step.Env[key] != want {
						t.Fatalf("worker input %s = %q, want %q", key, step.Env[key], want)
					}
				}
				process = true
			}
			if !process || checkouts != 1 {
				t.Fatal("worker invocation or unique source checkout missing")
			}
		})
	}
}

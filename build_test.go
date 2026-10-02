package infraci_test

import (
	"os"
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
		Concurrency struct {
			Group  string
			Cancel bool `yaml:"cancel-in-progress"`
			Queue  string
		}
		Jobs map[string]struct {
			Needs    any
			Name     string
			RunsOn   string `yaml:"runs-on"`
			Outputs  map[string]string
			Strategy struct{ Matrix string }
			Steps    []struct {
				Uses      string
				With, Env map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}

	t.Run("publication", func(t *testing.T) {
		if workflow.Concurrency.Group == "" || workflow.Concurrency.Cancel || workflow.Concurrency.Queue != "max" {
			t.Fatal("builds must queue under the publication lock")
		}
	})

	t.Run("admission", func(t *testing.T) {
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
	})

	actionPin := regexp.MustCompile(`^[^@]+@[a-f0-9]{40}$`)
	for _, name := range []string{"admit", "build", "builder"} {
		t.Run(name, func(t *testing.T) {
			job := workflow.Jobs[name]
			ref := "${{ needs.admit.outputs.revision }}"
			if name == "admit" {
				ref = "${{ inputs.source }}"
			}
			process := false
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
				case strings.HasPrefix(step.Uses, "actions/setup-go@"):
					if step.With["cache"] != "false" {
						t.Fatal("private source builds must not use the public Go cache")
					}
				case strings.HasPrefix(step.Uses, "cachix/install-nix-action@"):
					for _, setting := range []string{"sandbox = true", "accept-flake-config = false"} {
						if !strings.Contains(step.With["extra_nix_config"], setting) {
							t.Fatalf("missing worker Nix setting: %s", setting)
						}
					}
				}
				if name != "build" && step.Env["NIX_SIGNING_KEY"] != "" {
					t.Fatal("only coordinators may receive the cache signing key")
				}
				if step.Env["INPUT_MODE"] == "" {
					continue
				}
				if !strings.HasPrefix(step.Uses, "actions/github-script@") || step.With["script"] == "" {
					t.Fatal("worker requires a JavaScript action for job-scoped cache credentials")
				}
				process = step.Env["INPUT_MODE"] == name
			}
			if !process {
				t.Fatal("missing worker invocation")
			}
		})
	}
}

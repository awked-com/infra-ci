package infraci_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestWorkflowLauncher(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("workflow launcher tests require Node.js:", err)
	}
	data, err := os.ReadFile(filepath.Join(".github", "workflows", "build.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				ID   string
				With struct{ Script string }
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, step := range workflow.Jobs["admit"].Steps {
		if step.ID == "process" {
			script = step.With.Script
		}
	}
	if script == "" {
		t.Fatal("missing workflow launcher script")
	}
	encoded, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	fixtures := t.TempDir()
	launcher := filepath.Join(fixtures, "launcher.js")
	write(t, launcher, []byte(`
const fs = require('node:fs');
const AsyncFunction = Object.getPrototypeOf(async function() {}).constructor;
const core = {
  setSecret(value) { fs.writeFileSync(process.env.FIXTURE_DIRECTORY + '/masked', value); },
  setFailed(message) { console.error(message); process.exitCode = 1; }
};
new AsyncFunction('require', 'core', `+string(encoded)+`)(require, core).catch(() => {
  console.error('Launcher rejected.'); process.exitCode = 99;
});
`))
	compiler := filepath.Join(fixtures, "bin", "go")
	write(t, compiler, []byte("#!"+node+"\n"+workflowLauncherFixture))
	if err := os.Chmod(compiler, 0700); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name, mode string
		signal     syscall.Signal
		wantExit   int
	}{
		{name: "success", mode: "success"},
		{name: "compiler failure", mode: "compile-fail", wantExit: 1},
		{name: "worker failure", mode: "worker-fail", wantExit: 23},
		{name: "interrupt worker", mode: "worker-cancel", signal: syscall.SIGINT, wantExit: 130},
		{name: "terminate worker", mode: "worker-cancel", signal: syscall.SIGTERM, wantExit: 143},
		{name: "interrupt compiler", mode: "compile-cancel", signal: syscall.SIGINT, wantExit: 130},
		{name: "terminate compiler", mode: "compile-cancel", signal: syscall.SIGTERM, wantExit: 143},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "runner with spaces")
			write(t, filepath.Join(root, "source", "fixture"), nil)
			credentials := map[string]string{
				"ACTIONS_RUNTIME_TOKEN":          "fixture-runtime-secret",
				"ACTIONS_RESULTS_URL":            "https://results.invalid/fixture",
				"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "fixture-id-token-secret",
				"CI_IDENTITY":                    "fixture-identity-secret",
				"CI_STORAGE":                     "fixture-storage-secret",
				"REGISTRY_USER":                  "fixture-registry-user",
				"REGISTRY_TOKEN":                 "fixture-registry-secret",
				"GITHUB_TOKEN":                   "fixture-github-secret",
				"GH_TOKEN":                       "fixture-gh-secret",
				"NIX_SIGNING_KEY":                "fixture-signing-secret",
				"INPUT_GITHUB-TOKEN":             "fixture-duplicate-token-secret",
				"RUNNER_TRACKING_ID":             "fixture-tracking-id",
			}
			cmd := exec.Command(node, launcher)
			cmd.Env = append(os.Environ(),
				"PATH="+filepath.Dir(compiler)+string(os.PathListSeparator)+os.Getenv("PATH"),
				"FIXTURE_DIRECTORY="+root, "FIXTURE_MODE="+test.mode,
				"GITHUB_WORKSPACE="+root, "RUNNER_TEMP="+root, "GOTOOLCHAIN=auto",
			)
			keys := []string{"GOTOOLCHAIN"}
			for name, value := range credentials {
				cmd.Env = append(cmd.Env, name+"="+value)
				keys = append(keys, name)
			}
			cmd.Env = append(cmd.Env, "FIXTURE_KEYS="+strings.Join(keys, ","))
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			cmd.WaitDelay = time.Second
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			t.Cleanup(func() {
				// Each phase is a detached process group; kill it even if the launcher failed.
				for _, phase := range []string{"compile", "worker"} {
					if data, err := os.ReadFile(filepath.Join(root, phase+".pid")); err == nil {
						if pid, err := strconv.Atoi(string(data)); err == nil {
							_ = syscall.Kill(-pid, syscall.SIGKILL)
						}
					}
				}
				_ = cmd.Process.Kill()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("launcher did not stop during cleanup")
				}
			})
			if test.signal != 0 {
				ready := "descendant.ready"
				if test.mode == "compile-cancel" {
					ready = "compile.ready"
				}
				workflowWaitFile(t, filepath.Join(root, ready), done)
				if err := cmd.Process.Signal(test.signal); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("launcher did not finish; fixture normally waits one minute")
			}
			if got := cmd.ProcessState.ExitCode(); got != test.wantExit {
				t.Fatalf("exit %d, want %d: %s", got, test.wantExit, &output)
			}
			marker := regexp.MustCompile(`::stop-commands::([^\n]+)\n`).FindStringSubmatch(output.String())
			if len(marker) != 2 || !strings.Contains(output.String(), "\n::"+marker[1]+"::\n") {
				t.Fatalf("workflow command processing was not restored: %s", &output)
			}
			if strings.Contains(output.String(), "PRIVATE_COMPILER_OUTPUT") {
				t.Fatal("launcher exposed a private compiler diagnostic")
			}
			for _, private := range credentials {
				if strings.Contains(output.String(), private) {
					t.Fatal("launcher exposed a credential")
				}
			}
			log, err := os.ReadFile(filepath.Join(root, "infra-ci-compile.log"))
			if err != nil || !strings.Contains(string(log), "PRIVATE_COMPILER_OUTPUT") {
				t.Fatalf("private compiler diagnostics missing: %v", err)
			}
			info, err := os.Stat(filepath.Join(root, "infra-ci-compile.log"))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("compiler log must have mode 0600: %v", err)
			}
			masked, err := os.ReadFile(filepath.Join(root, "masked"))
			if err != nil || string(masked) != credentials["ACTIONS_RUNTIME_TOKEN"] {
				t.Fatalf("runtime token was not registered for masking: %v", err)
			}
			compiled := readWorkflowPhase(t, root, "compile")
			if !reflect.DeepEqual(compiled.Environment, map[string]string{"GOTOOLCHAIN": "local"}) {
				t.Fatal("compiler inherited worker credentials or did not use the installed Go toolchain")
			}
			if !reflect.DeepEqual(compiled.Arguments, []string{"-C", filepath.Join(root, "source"), "build", "-mod=readonly", "-o", filepath.Join(root, "nix-ci-worker"), "github.com/awked-com/nix-ci-worker/cmd/nix-ci-worker"}) {
				t.Fatalf("unexpected compiler arguments: %q", compiled.Arguments)
			}
			if strings.HasPrefix(test.mode, "compile-") {
				if _, err := os.Stat(filepath.Join(root, "worker.pid")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("worker started after failed or cancelled compilation")
				}
			} else {
				worker := readWorkflowPhase(t, root, "worker")
				delete(credentials, "INPUT_GITHUB-TOKEN")
				delete(credentials, "RUNNER_TRACKING_ID")
				credentials["GOTOOLCHAIN"] = "auto"
				if !reflect.DeepEqual(worker.Environment, credentials) {
					t.Fatal("worker did not receive its credentials or inherited redundant credentials")
				}
				for _, message := range []string{"WORKER_STDOUT", "WORKER_STDERR"} {
					if !strings.Contains(output.String(), message) {
						t.Fatalf("worker output was not streamed: %s", message)
					}
				}
			}
			if test.signal != 0 {
				phases := []string{"compile"}
				if test.mode == "worker-cancel" {
					phases = []string{"worker", "descendant"}
				}
				for _, phase := range phases {
					got, err := os.ReadFile(filepath.Join(root, phase+".signal"))
					if err != nil || string(got) != strconv.Itoa(int(test.signal)) {
						t.Fatalf("%s did not receive cancellation: %v", phase, err)
					}
					pid, err := os.ReadFile(filepath.Join(root, phase+".pid"))
					if err != nil {
						t.Fatal(err)
					}
					n, err := strconv.Atoi(string(pid))
					if err != nil || !errors.Is(syscall.Kill(n, 0), syscall.ESRCH) {
						t.Fatalf("%s survived launcher cancellation", phase)
					}
				}
			}
		})
	}
}

func workflowWaitFile(t *testing.T, path string, done <-chan struct{}) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-done:
			t.Fatal("launcher exited before the cancellation fixture started")
		case <-deadline.C:
			t.Fatal("cancellation fixture did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type workflowPhase struct {
	Environment map[string]string
	Arguments   []string
	Umask       int
	Core        string
}

func readWorkflowPhase(t *testing.T, root, phase string) workflowPhase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, phase+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var state workflowPhase
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.Umask != 0077 || state.Core != "0" {
		t.Fatalf("%s did not restrict file modes and core dumps", phase)
	}
	return state
}

const workflowLauncherFixture = `
const fs = require('node:fs'), path = require('node:path'), cp = require('node:child_process');
const phase = path.basename(__filename) === 'go' ? 'compile' : (process.argv[2] || 'worker');
const target = name => path.join(process.env.FIXTURE_DIRECTORY, name);
const write = (name, value) => fs.writeFileSync(target(name), String(value));
write(phase + '.pid', process.pid);
const keys = process.env.FIXTURE_KEYS.split(',');
write(phase + '.json', JSON.stringify({
  environment: Object.fromEntries(keys.map(key => [key, process.env[key]])),
  arguments: process.argv.slice(2), umask: process.umask(),
  core: cp.execFileSync('bash', ['-c', 'ulimit -c'], {encoding: 'utf8'}).trim()
}));
let descendant;
if (phase === 'compile') {
  console.log('PRIVATE_COMPILER_OUTPUT'); console.error('PRIVATE_COMPILER_OUTPUT');
  if (process.env.FIXTURE_MODE === 'compile-fail') process.exit(17);
  fs.copyFileSync(__filename, target('nix-ci-worker'));
  fs.chmodSync(target('nix-ci-worker'), 0o700);
} else if (phase === 'worker') {
  console.log('WORKER_STDOUT'); console.error('WORKER_STDERR');
  if (process.env.FIXTURE_MODE === 'worker-fail') process.exit(23);
  if (process.env.FIXTURE_MODE === 'worker-cancel') {
    descendant = cp.spawn(process.execPath, [__filename, 'descendant'], {stdio: 'inherit'});
  }
}
if (process.env.FIXTURE_MODE === phase + '-cancel' || phase === 'descendant') {
  const timer = setTimeout(() => process.exit(0), 60000);
  for (const [name, number] of [['SIGINT', 2], ['SIGTERM', 15]]) {
    process.on(name, () => {
      write(phase + '.signal', number);
      clearTimeout(timer);
      if (descendant && descendant.exitCode === null && descendant.signalCode === null) {
        descendant.once('exit', () => process.exit(0));
      } else process.exit(0);
    });
  }
  write(phase + '.ready', true);
}
`

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

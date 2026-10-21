package utils

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func fakeBin(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func resetTestKubeconfig(t *testing.T) {
	t.Helper()
	old := testKubeconfig
	testKubeconfig = ""
	t.Cleanup(func() { testKubeconfig = old })
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func kubeconfig(t *testing.T, currentContext string) string {
	t.Helper()
	return writeFile(t, "apiVersion: v1\nkind: Config\ncurrent-context: "+currentContext+"\n")
}

func TestRunRefusesEveryCommandWithoutTheTestKubeconfig(t *testing.T) {
	resetTestKubeconfig(t)
	marker := filepath.Join(t.TempDir(), "ran")
	for _, name := range []string{"kubectl", "make", "minikube"} {
		fakeBin(t, name, "touch "+marker)
		out, err := Run(exec.Command(name, "delete", "-f", "cert-manager.yaml"))
		if err == nil || !strings.Contains(err.Error(), "refusing to run "+name) {
			t.Fatalf("Run(%s) = %q, %v; want a refusal", name, out, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a refused command ran anyway (stat %s: %v)", marker, err)
	}
}

func TestRunGivesCommandsOnlyTheTestKubeconfig(t *testing.T) {
	resetTestKubeconfig(t)
	t.Setenv("KUBECONFIG", "/home/someone/.kube/production")
	t.Setenv("KUBECTL", "kubectl --kubeconfig=/home/someone/.kube/production")
	t.Setenv("MAKEFLAGS", "KUBECTL=kubectl --kubeconfig=/home/someone/.kube/production")
	t.Setenv("MFLAGS", "-k")
	t.Setenv("GNUMAKEFLAGS", "KUBECTL=kubectl\\ --context=production")
	t.Setenv("MAKEFILES", "/home/someone/override-kubectl.mk")
	testKubeconfig = filepath.Join(t.TempDir(), "e2e-kubeconfig")
	fakeBin(t, "kubectl", `printf '%s|%s|%s|%s|%s|%s' "$KUBECONFIG" "$KUBECTL" "$MAKEFLAGS" "$MFLAGS" "$GNUMAKEFLAGS" "$MAKEFILES"`)
	out, err := Run(exec.Command("kubectl", "get", "ns"))
	if err != nil {
		t.Fatal(err)
	}
	if want := testKubeconfig + "|kubectl||||"; out != want {
		t.Fatalf("kubectl saw KUBECONFIG|KUBECTL|MAKEFLAGS|MFLAGS|GNUMAKEFLAGS|MAKEFILES = %q, want %q", out, want)
	}
}

func TestUseTestKubeconfigRejectsAnythingButTheMinikubeProfile(t *testing.T) {
	for _, tc := range []struct {
		name, path, wantErr string
	}{
		{"no kubeconfig", "", "E2E_KUBECONFIG is not set"},
		{"missing file", filepath.Join(t.TempDir(), "absent"), "reading the e2e kubeconfig"},
		{"not yaml", writeFile(t, "{"), "parsing the e2e kubeconfig"},
		{"other context", kubeconfig(t, "pdk"),
			`has current-context "pdk", not the minikube profile "endpoint-monitoring-operator-e2e"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetTestKubeconfig(t)
			t.Setenv("E2E_KUBECONFIG", tc.path)
			err := UseTestKubeconfig()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("UseTestKubeconfig() = %v, want an error containing %q", err, tc.wantErr)
			}
			if testKubeconfig != "" {
				t.Fatalf("an error left testKubeconfig=%q", testKubeconfig)
			}
		})
	}

	t.Run("matching profile", func(t *testing.T) {
		resetTestKubeconfig(t)
		good := kubeconfig(t, TestProfile)
		t.Setenv("E2E_KUBECONFIG", good)
		if err := UseTestKubeconfig(); err != nil {
			t.Fatal(err)
		}
		if testKubeconfig != good {
			t.Fatalf("testKubeconfig=%q, want %q", testKubeconfig, good)
		}
	})

	t.Run("relative path", func(t *testing.T) {
		resetTestKubeconfig(t)
		good := kubeconfig(t, TestProfile)
		t.Chdir(filepath.Dir(good))
		t.Setenv("E2E_KUBECONFIG", filepath.Base(good))
		if err := UseTestKubeconfig(); err != nil {
			t.Fatal(err)
		}
		if testKubeconfig != good {
			t.Fatalf("testKubeconfig=%q, want the absolute %q", testKubeconfig, good)
		}
	})
}

func TestLoadImageToMinikubeUsesTheTestProfile(t *testing.T) {
	resetTestKubeconfig(t)
	testKubeconfig = filepath.Join(t.TempDir(), "e2e-kubeconfig")
	args := filepath.Join(t.TempDir(), "args")
	fakeBin(t, "minikube", `printf '%s ' "$@" > `+args)
	if err := LoadImageToMinikube("example.com/img:v1"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if want := "-p endpoint-monitoring-operator-e2e image load example.com/img:v1"; strings.TrimSpace(string(got)) != want {
		t.Fatalf("minikube got %q, want %q", strings.TrimSpace(string(got)), want)
	}
}

func TestMakefilePinsTheE2EVariables(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"E2E_PROFILE":    TestProfile,
		"E2E_KUBECONFIG": "$(CURDIR)/bin/e2e-kubeconfig",
		"MINIKUBE":       "minikube",
	} {
		m := regexp.MustCompile(`(?m)^override ` + name + ` := (\S+)$`).FindSubmatch(data)
		if m == nil {
			t.Errorf("the Makefile does not pin %s with an override := line", name)
			continue
		}
		if string(m[1]) != want {
			t.Errorf("the Makefile's %s is %q, want %q", name, m[1], want)
		}
	}
}

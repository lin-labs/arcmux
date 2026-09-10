package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func readRepoFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{".."}, parts...)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestSystemdUnitPreservesTmuxAcrossDaemonRestart(t *testing.T) {
	unit := readRepoFile(t, "packaging", "systemd", "arcmux.service")
	if !strings.Contains(unit, "KillMode=process") {
		t.Fatal("systemd unit must restart only arcmux; tmux is durable child state")
	}
	if !strings.Contains(unit, "ExecStart=@BIN@ start") {
		t.Fatal("systemd unit must render the installed arcmux binary")
	}
	if !strings.Contains(unit, "WorkingDirectory=@HOME@") {
		t.Fatal("systemd unit must not pin a disposable release worktree")
	}
}

func TestServiceInstallSupportsLinuxAndDarwin(t *testing.T) {
	makefile := readRepoFile(t, "Makefile")
	for _, want := range []string{
		"packaging/systemd/$(BINARY).service",
		"uname -s",
		"Linux)",
		"Darwin)",
		"@set -e; case \"$$(uname -s)\" in",
	} {
		if !strings.Contains(makefile, want) {
			t.Fatalf("Makefile service-install is missing %q", want)
		}
	}
}

func TestLinuxReleaseDependsOnServiceInstall(t *testing.T) {
	makefile := readRepoFile(t, "Makefile")
	for _, want := range []string{
		"service-install: install",
		"ifeq ($(shell uname -s),Linux)\nrelease: service-install\nelse\nrelease: install restart\nendif",
	} {
		if !strings.Contains(makefile, want) {
			t.Fatalf("Makefile release lifecycle is missing %q", want)
		}
	}

	linuxBranch := strings.Index(makefile, "Linux) \\")
	renderUnit := strings.Index(makefile[linuxBranch:], "$(SYSTEMD_TEMPLATE) > $(SYSTEMD_UNIT)")
	restartUnit := strings.Index(makefile[linuxBranch:], "systemctl --user restart $(BINARY).service")
	if renderUnit < 0 || restartUnit < 0 || renderUnit >= restartUnit {
		t.Fatal("Linux service install must render the safe unit before restarting")
	}
}

func TestLaunchdServiceStillUsesKeepAlive(t *testing.T) {
	plist := readRepoFile(t, "packaging", "launchd", "com.blin.arcmux.plist")
	if !strings.Contains(plist, "<key>KeepAlive</key>\n  <true/>") {
		t.Fatal("launchd service must retain KeepAlive restart supervision")
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func serviceFixture(t *testing.T, platform, active, target string) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	for _, path := range []string{
		".gitignore",
		"Makefile",
		"packaging/launchd/com.blin.arcmux.plist",
		"packaging/systemd/arcmux.service",
	} {
		data := readRepoFile(t, strings.Split(path, "/")...)
		target := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Arcmux Test", "GIT_AUTHOR_EMAIL=arcmux@example.invalid",
		"GIT_COMMITTER_NAME=Arcmux Test", "GIT_COMMITTER_EMAIL=arcmux@example.invalid",
	)
	for _, args := range [][]string{{"init", "-b", "main"}, {"add", "."}, {"commit", "-m", "fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	revisionCmd := exec.Command("git", "rev-parse", "HEAD")
	revisionCmd.Dir = root
	revisionBytes, err := revisionCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	fixtureRevision := strings.TrimSpace(string(revisionBytes))

	fakeBin := filepath.Join(root, ".git", "test-bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "uname"), "printf '%s\\n' \"$FAKE_OS\"\n")
	writeExecutable(t, filepath.Join(fakeBin, "go"),
		"[ \"${1:-}\" = test ] && exit 0\n"+
			"out=\n"+
			"while [ \"$#\" -gt 0 ]; do\n"+
			"  if [ \"$1\" = \"-o\" ]; then out=\"$2\"; break; fi\n"+
			"  shift\n"+
			"done\n"+
			"mkdir -p \"$(dirname \"$out\")\"\n"+
			"if [ \"$(basename \"$out\")\" = arcmux ]; then\n"+
			"  cat > \"$out\" <<'ARCMUX_TEST_BINARY'\n"+
			"#!/bin/sh\n"+
			"printf '%s\\n' '{\"version\":\"0.1.0\",\"revision\":\""+fixtureRevision+"\",\"build_time\":\"test\",\"summarizer_safety\":\"direct-api-only-v1\"}'\n"+
			"ARCMUX_TEST_BINARY\n"+
			"else\n"+
			"  : > \"$out\"\n"+
			"fi\n"+
			"chmod 0755 \"$out\"\n")
	writeExecutable(t, filepath.Join(fakeBin, "launchctl"),
		"printf 'launchctl %s\\n' \"$*\" >> \"$SERVICE_TEST_LOG\"\n"+
			"case \" $* \" in *\" print \"*) [ \"$FAKE_ACTIVE\" = 1 ]; exit ;; esac\n")
	writeExecutable(t, filepath.Join(fakeBin, "pkill"),
		"printf 'pkill %s\\n' \"$*\" >> \"$SERVICE_TEST_LOG\"\n"+
			"exit 1\n")
	writeExecutable(t, filepath.Join(fakeBin, "pgrep"),
		"printf 'pgrep %s\\n' \"$*\" >> \"$SERVICE_TEST_LOG\"\n"+
			"exit 1\n")
	writeExecutable(t, filepath.Join(fakeBin, "sleep"),
		"printf 'sleep %s\\n' \"$*\" >> \"$SERVICE_TEST_LOG\"\n")
	writeExecutable(t, filepath.Join(fakeBin, "systemctl"),
		"printf 'systemctl %s\\n' \"$*\" >> \"$SERVICE_TEST_LOG\"\n"+
			"case \" $* \" in\n"+
			"  *\" is-active \"*) [ \"$FAKE_ACTIVE\" = 1 ]; exit ;;\n"+
			"  *\" list-unit-files \"*) printf '%s\\n' 'arcmux.service disabled enabled' ;;\n"+
			"  *\" show \"*\" LoadState \"*) printf '%s\\n' \"$FAKE_LOAD_STATE\" ;;\n"+
			"  *\" disable \"*) [ \"$FAKE_DISABLE_FAIL\" != 1 ] || exit 42 ;;\n"+
			"esac\n"+
			"exit 0\n")

	logPath := filepath.Join(root, ".git", "service.log")
	home := filepath.Join(root, ".git", "test-home")
	cmd := exec.Command("make", target)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PATH="+fakeBin+":/usr/bin:/bin",
		"FAKE_OS="+platform,
		"FAKE_ACTIVE="+active,
		"FAKE_LOAD_STATE=loaded",
		"SERVICE_TEST_LOG="+logPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make %s: %v\n%s", target, err, out)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	return root, home, string(logData)
}

func TestLinuxServiceInstallRendersUnitAndPreservesInactiveState(t *testing.T) {
	_, home, log := serviceFixture(t, "Linux", "0", "service-install")
	unitPath := filepath.Join(home, ".config", "systemd", "user", "arcmux.service")
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(unit)
	if strings.Contains(rendered, "@HOME@") ||
		strings.Contains(rendered, "@BIN@") ||
		strings.Contains(rendered, "@PATH@") ||
		!strings.Contains(rendered, "KillMode=process") {
		t.Fatalf("unsafe rendered unit:\n%s", rendered)
	}
	if strings.Contains(log, "pkill ") || strings.Contains(log, "systemctl --user restart arcmux.service") ||
		strings.Contains(log, "systemctl --user enable arcmux.service") {
		t.Fatalf("inactive service was activated or process state was mutated:\n%s", log)
	}

	if analyze, err := exec.LookPath("systemd-analyze"); err == nil {
		cmd := exec.Command(analyze, "--user", "verify", unitPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("systemd-analyze verify: %v\n%s", err, out)
		}
	}
}

func TestLinuxReleaseRefreshesUnitBeforeRestart(t *testing.T) {
	_, home, log := serviceFixture(t, "Linux", "1", "release")
	if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", "arcmux.service")); err != nil {
		t.Fatal(err)
	}
	reload := strings.Index(log, "systemctl --user daemon-reload")
	restart := strings.Index(log, "systemctl --user restart arcmux.service")
	if reload < 0 || restart < 0 || reload >= restart {
		t.Fatalf("release did not refresh the unit before restart:\n%s", log)
	}
}

// Boot durability, not just "is it running now". On 2026-09-10 labs came up
// from a reboot with no arcmux: the unit was loaded but disabled, so nothing
// started it. `Restart=always` only covers crashes.
func TestLinuxStartEnablesUnitForBoot(t *testing.T) {
	_, _, log := serviceFixture(t, "Linux", "0", "start")
	if !strings.Contains(log, "systemctl --user enable --now arcmux") {
		t.Fatalf("make start must enable the unit so arcmux survives reboot:\n%s", log)
	}
}

// A box that is running arcmux from a disabled unit is one reboot from silence.
// Releasing onto it must heal that, without activating a fresh inactive install.
func TestLinuxServiceInstallEnablesActiveUnitForBoot(t *testing.T) {
	_, _, log := serviceFixture(t, "Linux", "1", "service-install")
	enable := strings.Index(log, "systemctl --user enable arcmux.service")
	restart := strings.Index(log, "systemctl --user restart arcmux.service")
	if enable < 0 || restart < 0 || enable >= restart {
		t.Fatalf("active service-install must enable the unit before restarting:\n%s", log)
	}
}

func TestLinuxServiceInstallDoesNotStopActiveManagedDaemon(t *testing.T) {
	_, _, log := serviceFixture(t, "Linux", "1", "service-install")
	if strings.Contains(log, "pkill ") {
		t.Fatalf("active managed daemon was stopped outside systemd:\n%s", log)
	}
}

func TestDarwinServiceInstallRetainsLaunchdPath(t *testing.T) {
	_, home, log := serviceFixture(t, "Darwin", "0", "service-install")
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.blin.arcmux.plist")
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plist), "@HOME@") ||
		strings.Contains(string(plist), "@BIN@") ||
		strings.Contains(string(plist), "@LOG_FILE@") ||
		strings.Contains(string(plist), "@PATH@") ||
		!strings.Contains(string(plist), "<key>KeepAlive</key>") {
		t.Fatalf("invalid rendered launchd plist:\n%s", plist)
	}
	if strings.Contains(log, "launchctl bootstrap") || strings.Contains(log, "systemctl") {
		t.Fatalf("inactive Darwin service was loaded or used the wrong supervisor:\n%s", log)
	}
}

func TestLinuxServiceUninstallSurfacesDisableFailure(t *testing.T) {
	root, home, _ := serviceFixture(t, "Linux", "0", "service-install")
	cmd := exec.Command("make", "service-uninstall")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PATH="+filepath.Join(root, ".git", "test-bin")+":/usr/bin:/bin",
		"FAKE_OS=Linux",
		"FAKE_ACTIVE=0",
		"FAKE_LOAD_STATE=loaded",
		"FAKE_DISABLE_FAIL=1",
		"SERVICE_TEST_LOG="+filepath.Join(root, "service.log"),
	)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("service-uninstall hid systemctl failure:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", "arcmux.service")); err != nil {
		t.Fatal("failed uninstall must leave the unit in place")
	}
}

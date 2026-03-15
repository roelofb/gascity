package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestDoSupervisorLogsNoFile(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := doSupervisorLogs(50, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doSupervisorLogs code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "log file not found") {
		t.Fatalf("stderr = %q, want missing log file message", stderr.String())
	}
}

func TestRenderSupervisorLaunchdTemplate(t *testing.T) {
	data := &supervisorServiceData{
		GCPath:  "/usr/local/bin/gc",
		LogPath: "/home/user/.gc/supervisor.log",
		GCHome:  "/home/user/.gc",
	}

	content, err := renderSupervisorTemplate(supervisorLaunchdTemplate, data)
	if err != nil {
		t.Fatal(err)
	}

	for _, check := range []string{
		"com.gascity.supervisor",
		"/usr/local/bin/gc",
		"supervisor",
		"run",
		"/home/user/.gc/supervisor.log",
		"GC_HOME",
	} {
		if !strings.Contains(content, check) {
			t.Fatalf("launchd template missing %q", check)
		}
	}
}

func TestRenderSupervisorSystemdTemplate(t *testing.T) {
	data := &supervisorServiceData{
		GCPath:  "/usr/local/bin/gc",
		LogPath: "/home/user/.gc/supervisor.log",
		GCHome:  "/home/user/.gc",
	}

	content, err := renderSupervisorTemplate(supervisorSystemdTemplate, data)
	if err != nil {
		t.Fatal(err)
	}

	for _, check := range []string{
		"[Service]",
		`ExecStart=/usr/local/bin/gc supervisor run`,
		`StandardOutput=append:/home/user/.gc/supervisor.log`,
		`Environment=GC_HOME="/home/user/.gc"`,
	} {
		if !strings.Contains(content, check) {
			t.Fatalf("systemd template missing %q", check)
		}
	}
}

func TestSupervisorInstallUnsupportedOS(t *testing.T) {
	if goruntime.GOOS == "darwin" || goruntime.GOOS == "linux" {
		t.Skip("unsupported-os test only applies outside darwin/linux")
	}
	t.Setenv("GC_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := doSupervisorInstall(&stdout, &stderr)
	if code != 1 {
		t.Fatalf("doSupervisorInstall code = %d, want 1", code)
	}
}

func TestDoSupervisorStartAlreadyRunning(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	lock, err := acquireSupervisorLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // test cleanup

	var stdout, stderr bytes.Buffer
	code := doSupervisorStart(&stdout, &stderr)
	if code != 1 {
		t.Fatalf("doSupervisorStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already running") {
		t.Fatalf("stderr = %q, want already running message", stderr.String())
	}
}

func TestRunSupervisorFailsWhenAPIPortUnavailable(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close() //nolint:errcheck

	port := lis.Addr().(*net.TCPAddr).Port
	cfg := []byte("[supervisor]\nport = " + strconv.Itoa(port) + "\n")
	if err := os.WriteFile(supervisor.ConfigPath(), cfg, 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runSupervisor(&stdout, &stderr)
	if code != 1 {
		t.Fatalf("runSupervisor code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "api: listen") {
		t.Fatalf("stderr = %q, want API listen failure", stderr.String())
	}
}

func TestControllerStatusForSupervisorManagedCity(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	old := supervisorAliveHook
	supervisorAliveHook = func() int { return 4242 }
	defer func() { supervisorAliveHook = old }()

	ctrl := controllerStatusForCity(cityPath)
	if !ctrl.Running || ctrl.PID != 4242 || ctrl.Mode != "supervisor" {
		t.Fatalf("controller status = %+v, want running supervisor PID", ctrl)
	}
}

func TestSupervisorCityAPIClientRequiresRunning(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	supervisorAliveHook = func() int { return 4242 }
	supervisorCityRunningHook = func(string) (bool, bool) { return false, true }
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	})

	if client := supervisorCityAPIClient(cityPath); client != nil {
		t.Fatalf("supervisorCityAPIClient(%q) = %#v, want nil for stopped city", cityPath, client)
	}
}

func TestControllerAliveNoSocket(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := controllerAlive(dir); got != 0 {
		t.Fatalf("controllerAlive = %d, want 0", got)
	}
}

func TestStartHiddenLegacyFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newStartCmd(&stdout, &stderr)

	for _, name := range []string{"foreground", "controller", "file", "no-strict"} {
		flag := cmd.Flags().Lookup(name)
		if flag == nil {
			t.Fatalf("missing %s flag", name)
		}
		if !flag.Hidden {
			t.Fatalf("%s flag should be hidden", name)
		}
	}

	if flag := cmd.Flags().Lookup("dry-run"); flag == nil || flag.Hidden {
		t.Fatal("dry-run flag should remain visible")
	}
}

func TestDoStartRequiresInitializedCity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doStart([]string{dir}, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not in a city directory") {
		t.Fatalf("stderr = %q, want city-directory error", stderr.String())
	}
	if !strings.Contains(stderr.String(), `gc init `+dir) {
		t.Fatalf("stderr = %q, want init guidance", stderr.String())
	}
}

func TestDoStartRejectsUnbootstrappedCityConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doStart([]string{dir}, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "city runtime not bootstrapped") {
		t.Fatalf("stderr = %q, want bootstrap error", stderr.String())
	}
	if !strings.Contains(stderr.String(), `gc init `+dir) {
		t.Fatalf("stderr = %q, want init guidance", stderr.String())
	}
}

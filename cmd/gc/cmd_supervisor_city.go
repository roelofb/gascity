package main

import (
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/supervisor"
)

var (
	supervisorCityReadyTimeout = 5 * time.Second
	supervisorCityPollInterval = 100 * time.Millisecond
)

func effectiveCityName(cityPath string) (string, error) {
	name := filepath.Base(cityPath)
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	if err != nil {
		return "", err
	}
	if cfg.Workspace.Name != "" {
		name = cfg.Workspace.Name
	}
	return name, nil
}

func normalizeRegisteredCityPath(cityPath string) (string, error) {
	abs, err := filepath.Abs(cityPath)
	if err != nil {
		return "", err
	}
	if resolved, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		abs = resolved
	}
	return abs, nil
}

func registeredCityEntry(cityPath string) (supervisor.CityEntry, bool, error) {
	normalized, err := normalizeRegisteredCityPath(cityPath)
	if err != nil {
		return supervisor.CityEntry{}, false, err
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		return supervisor.CityEntry{}, false, err
	}
	for _, entry := range entries {
		if entry.Path == normalized {
			return entry, true, nil
		}
	}
	return supervisor.CityEntry{}, false, nil
}

func registerCityWithSupervisor(cityPath string, stdout, stderr io.Writer, commandName string) int {
	name, err := effectiveCityName(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, name); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	entry, _, err := registeredCityEntry(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Registered city '%s' (%s)\n", entry.EffectiveName(), entry.Path) //nolint:errcheck // best-effort stdout

	if ensureSupervisorRunningHook(stdout, stderr) != 0 {
		rollbackRegisteredCity(reg, entry, stderr, commandName, "supervisor did not start")
		return 1
	}
	if reloadSupervisorHook(stdout, stderr) != 0 {
		rollbackRegisteredCity(reg, entry, stderr, commandName, "reconcile failed")
		return 1
	}
	if supervisorAliveHook() != 0 {
		if err := waitForSupervisorCity(cityPath, true, supervisorCityReadyTimeout); err != nil {
			rollbackRegisteredCity(reg, entry, stderr, commandName, err.Error())
			fmt.Fprintf(stderr, "%s: check 'gc supervisor logs' for details\n", commandName) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	return 0
}

func rollbackRegisteredCity(reg *supervisor.Registry, entry supervisor.CityEntry, stderr io.Writer, commandName, reason string) {
	if err := reg.Unregister(entry.Path); err != nil {
		fmt.Fprintf(stderr, "%s: %s; rollback failed for '%s': %v\n", commandName, reason, entry.Path, err) //nolint:errcheck // best-effort stderr
		return
	}
	fmt.Fprintf(stderr, "%s: %s; registration rolled back for '%s'\n", commandName, reason, entry.EffectiveName()) //nolint:errcheck // best-effort stderr
}

func waitForSupervisorCity(cityPath string, wantRunning bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		running, known := supervisorCityRunningHook(cityPath)
		if known {
			if running == wantRunning {
				return nil
			}
			if !wantRunning {
				return fmt.Errorf("city is still running under supervisor")
			}
		} else if !wantRunning {
			return nil
		}
		if time.Now().After(deadline) {
			if wantRunning {
				return fmt.Errorf("city did not become ready under supervisor")
			}
			return fmt.Errorf("city did not stop under supervisor")
		}
		time.Sleep(supervisorCityPollInterval)
	}
}

func unregisterCityFromSupervisor(cityPath string, stdout, stderr io.Writer, commandName string) (bool, int) {
	entry, registered, err := registeredCityEntry(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return false, 1
	}
	if !registered {
		return false, 0
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Unregister(cityPath); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", commandName, err) //nolint:errcheck // best-effort stderr
		return true, 1
	}

	fmt.Fprintf(stdout, "Unregistered city '%s' (%s)\n", entry.EffectiveName(), entry.Path) //nolint:errcheck // best-effort stdout

	if supervisorAliveHook() != 0 {
		if reloadSupervisorHook(stdout, stderr) != 0 {
			if reErr := reg.Register(entry.Path, entry.EffectiveName()); reErr != nil {
				fmt.Fprintf(stderr, "%s: reconcile failed and restore failed for '%s': %v\n", commandName, entry.EffectiveName(), reErr) //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "%s: reconcile failed; restored registration for '%s'\n", commandName, entry.EffectiveName()) //nolint:errcheck
			}
			return true, 1
		}
		if err := waitForSupervisorCity(cityPath, false, supervisorCityReadyTimeout); err != nil {
			if reErr := reg.Register(entry.Path, entry.EffectiveName()); reErr != nil {
				fmt.Fprintf(stderr, "%s: %v; restore failed for '%s': %v\n", commandName, err, entry.EffectiveName(), reErr) //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "%s: %v; restored registration for '%s'\n", commandName, err, entry.EffectiveName()) //nolint:errcheck
			}
			return true, 1
		}
	}
	return true, 0
}

func supervisorAPIBaseURL() (string, error) {
	cfg, err := supervisor.LoadConfig(supervisor.ConfigPath())
	if err != nil {
		return "", err
	}
	bind := cfg.Supervisor.BindOrDefault()
	switch bind {
	case "0.0.0.0":
		bind = "127.0.0.1"
	case "::", "[::]":
		bind = "::1"
	}
	return fmt.Sprintf("http://%s", net.JoinHostPort(bind, strconv.Itoa(cfg.Supervisor.PortOrDefault()))), nil
}

var supervisorCityRunningHook = supervisorCityRunning

func supervisorCityAPIClient(cityPath string) *api.Client {
	entry, registered, err := registeredCityEntry(cityPath)
	if err != nil || !registered || supervisorAliveHook() == 0 {
		return nil
	}
	if running, known := supervisorCityRunningHook(cityPath); !known || !running {
		return nil
	}
	baseURL, err := supervisorAPIBaseURL()
	if err != nil {
		return nil
	}
	return api.NewCityScopedClient(baseURL, entry.EffectiveName())
}

func supervisorCityRunning(cityPath string) (bool, bool) {
	if supervisorAliveHook() == 0 {
		return false, false
	}
	baseURL, err := supervisorAPIBaseURL()
	if err != nil {
		return false, false
	}
	client := api.NewClient(baseURL)
	cities, err := client.ListCities()
	if err != nil {
		return false, false
	}
	normalized, err := normalizeRegisteredCityPath(cityPath)
	if err != nil {
		return false, false
	}
	for _, city := range cities {
		path, pathErr := normalizeRegisteredCityPath(city.Path)
		if pathErr == nil && path == normalized {
			return city.Running, true
		}
	}
	return false, false
}

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"syscall"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"golang.org/x/xerrors"
)

var (
	fuseDeviceMajor int64 = 10
	fuseDeviceMinor int64 = 229
)

func main() {
	log := logrus.New()
	log.SetLevel(logrus.DebugLevel)

	var err error
	runcPath, err := exec.LookPath("runc.orig")
	if err != nil {
		log.WithError(err).Fatal("runc not found")
	}

	var useFacade bool
	for _, arg := range os.Args {
		if arg == "create" {
			useFacade = true
			break
		}
	}

	if useFacade {
		err = createAndRunc(runcPath, log)
	} else {
		err = syscall.Exec(runcPath, os.Args, os.Environ())
	}
	if err != nil {
		log.WithError(err).Fatal("failed")
	}
}

func createAndRunc(runcPath string, log *logrus.Logger) error {
	fc, err := os.ReadFile("config.json")
	if err != nil {
		return xerrors.Errorf("cannot read config.json: %w", err)
	}

	var cfg specs.Spec
	err = json.Unmarshal(fc, &cfg)
	if err != nil {
		return xerrors.Errorf("cannot decode config.json: %w", err)
	}

	fuseDevice := specs.LinuxDeviceCgroup{
		Type:   "c",
		Minor:  &fuseDeviceMinor,
		Major:  &fuseDeviceMajor,
		Access: "rwm",
		Allow:  true,
	}

	cfg.Linux.Resources.Devices = append(cfg.Linux.Resources.Devices, fuseDevice)

	err = syscall.Exec(runcPath, os.Args, os.Environ())
	if err != nil {
		return xerrors.Errorf("exec %s: %w", runcPath, err)
	}

	return nil
}

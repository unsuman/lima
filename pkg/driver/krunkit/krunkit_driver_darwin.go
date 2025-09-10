// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package krunkit

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/lima-vm/lima/v2/pkg/driver"
	"github.com/lima-vm/lima/v2/pkg/executil"
	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/limatype/filenames"
	"github.com/lima-vm/lima/v2/pkg/networks/usernet"
	"github.com/lima-vm/lima/v2/pkg/ptr"
)

type LimaKrunkitDriver struct {
	Instance     *limatype.Instance
	SSHLocalPort int

	usernetClient *usernet.Client
	stopUsernet   context.CancelFunc
	krunkitCmd    *exec.Cmd
	krunkitWaitCh chan error
}

var _ driver.Driver = (*LimaKrunkitDriver)(nil)

func New() *LimaKrunkitDriver {
	return &LimaKrunkitDriver{}
}

func (l *LimaKrunkitDriver) Configure(inst *limatype.Instance) *driver.ConfiguredDriver {
	l.Instance = inst
	l.SSHLocalPort = inst.SSHLocalPort

	return &driver.ConfiguredDriver{
		Driver: l,
	}
}

func (l *LimaKrunkitDriver) CreateDisk(ctx context.Context) error {
	return EnsureDisk(ctx, l.Instance)
}

func (l *LimaKrunkitDriver) Start(ctx context.Context) (chan error, error) {
	var err error
	l.usernetClient, l.stopUsernet, err = startUsernet(ctx, l.Instance)
	if err != nil {
		return nil, fmt.Errorf("failed to start usernet: %w", err)
	}

	krunkitCmd, err := Cmdline(l.Instance)
	if err != nil {
		return nil, fmt.Errorf("failed to construct krunkit command line: %w", err)
	}
	// Detach krunkit process from parent Lima process
	krunkitCmd.SysProcAttr = executil.BackgroundSysProcAttr

	logPath := filepath.Join(l.Instance.Dir, "krunkit.log")
	logfile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open krunkit logfile: %w", err)
	}
	krunkitCmd.Stderr = logfile

	logrus.Infof("Starting krun VM (hint: to watch the progress, see %q)", logPath)
	logrus.Debugf("krunkitCmd.Args: %v", krunkitCmd.Args)

	if err := krunkitCmd.Start(); err != nil {
		logfile.Close()
		return nil, fmt.Errorf("failed to start krunkitCmd")
	}

	pidPath := filepath.Join(l.Instance.Dir, filenames.PIDFile(*l.Instance.Config.VMType))
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", krunkitCmd.Process.Pid)), 0644); err != nil {
		logrus.WithError(err).Warn("Failed to write PID file")
	}

	l.krunkitCmd = krunkitCmd
	l.krunkitWaitCh = make(chan error, 1)
	go func() {
		defer func() {
			logfile.Close()
			os.RemoveAll(pidPath)
			close(l.krunkitWaitCh)
		}()
		l.krunkitWaitCh <- krunkitCmd.Wait()
	}()

	err = l.usernetClient.ConfigureDriver(ctx, l.Instance, l.SSHLocalPort)
	if err != nil {
		l.krunkitWaitCh <- fmt.Errorf("failed to configure usernet: %w", err)
	}

	return l.krunkitWaitCh, nil
}

func (l *LimaKrunkitDriver) Stop(ctx context.Context) error {
	if l.krunkitCmd == nil {
		return nil
	}

	if err := l.krunkitCmd.Process.Signal(syscall.SIGTERM); err != nil {
		logrus.WithError(err).Warn("Failed to send interrupt signal")
	}

	go func() {
		if l.usernetClient != nil {
			_ = l.usernetClient.UnExposeSSH(l.Instance.SSHLocalPort)
		}
		if l.stopUsernet != nil {
			l.stopUsernet()
		}
	}()

	timeout := time.After(30 * time.Second)
	select {
	case <-l.krunkitWaitCh:
		return nil
	case <-timeout:
		if err := l.krunkitCmd.Process.Kill(); err != nil {
			return err
		}

		<-l.krunkitWaitCh
		return nil
	}
}

func (l *LimaKrunkitDriver) Delete(ctx context.Context) error {
	return nil
}

func (l *LimaKrunkitDriver) InspectStatus(ctx context.Context, inst *limatype.Instance) string {
	return ""
}

func (l *LimaKrunkitDriver) RunGUI() error {
	return nil
}

func (l *LimaKrunkitDriver) ChangeDisplayPassword(ctx context.Context, password string) error {
	return fmt.Errorf("display password change not supported by krun driver")
}

func (l *LimaKrunkitDriver) DisplayConnection(ctx context.Context) (string, error) {
	return "", fmt.Errorf("display connection not supported by krun driver")
}

func (l *LimaKrunkitDriver) CreateSnapshot(ctx context.Context, tag string) error {
	return fmt.Errorf("snapshots not supported by krun driver")
}

func (l *LimaKrunkitDriver) ApplySnapshot(ctx context.Context, tag string) error {
	return fmt.Errorf("snapshots not supported by krun driver")
}

func (l *LimaKrunkitDriver) DeleteSnapshot(ctx context.Context, tag string) error {
	return fmt.Errorf("snapshots not supported by krun driver")
}

func (l *LimaKrunkitDriver) ListSnapshots(ctx context.Context) (string, error) {
	return "", fmt.Errorf("snapshots not supported by krun driver")
}

func (l *LimaKrunkitDriver) Register(ctx context.Context) error {
	return nil
}

func (l *LimaKrunkitDriver) Unregister(ctx context.Context) error {
	return nil
}

func (l *LimaKrunkitDriver) ForwardGuestAgent() bool {
	return true
}

func (l *LimaKrunkitDriver) GuestAgentConn(ctx context.Context) (net.Conn, string, error) {
	return nil, "unix", nil
}

func (l *LimaKrunkitDriver) Validate(ctx context.Context) error {
	return nil
}

func (l *LimaKrunkitDriver) FillConfig(ctx context.Context, cfg *limatype.LimaYAML, filePath string) error {
	if cfg.MountType == nil {
		cfg.MountType = ptr.Of(limatype.VIRTIOFS)
	} else {
		*cfg.MountType = limatype.VIRTIOFS
	}

	cfg.VMType = ptr.Of("krunkit")

	return nil
}

func (l *LimaKrunkitDriver) BootScripts() (map[string][]byte, error) {
	return nil, nil
}

func (l *LimaKrunkitDriver) Create(ctx context.Context) error {
	return nil
}

func (l *LimaKrunkitDriver) Info() driver.Info {
	var info driver.Info
	info.Name = "krunkit"
	if l.Instance != nil && l.Instance.Dir != "" {
		info.InstanceDir = l.Instance.Dir
	}

	info.Features = driver.DriverFeatures{
		DynamicSSHAddress:    false,
		SkipSocketForwarding: false,
		CanRunGUI:            false,
	}
	return info
}

func (l *LimaKrunkitDriver) SSHAddress(ctx context.Context) (string, error) {
	return "127.0.0.1", nil
}

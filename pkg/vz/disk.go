// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package vz

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/docker/go-units"

	"github.com/lima-vm/lima/pkg/driver"
	"github.com/lima-vm/lima/pkg/fileutils"
	"github.com/lima-vm/lima/pkg/imgutil/proxyimgutil"
	"github.com/lima-vm/lima/pkg/iso9660util"
	"github.com/lima-vm/lima/pkg/store/filenames"
)

func EnsureDisk(ctx context.Context, driver *driver.BaseDriver) error {
	diffDisk := filepath.Join(driver.Instance.Dir, filenames.DiffDisk)
	if _, err := os.Stat(diffDisk); err == nil || !errors.Is(err, os.ErrNotExist) {
		// disk is already ensured
		return err
	}

	diskUtil := proxyimgutil.NewDiskUtil()

	baseDisk := filepath.Join(driver.Instance.Dir, filenames.BaseDisk)
	kernel := filepath.Join(driver.Instance.Dir, filenames.Kernel)
	kernelCmdline := filepath.Join(driver.Instance.Dir, filenames.KernelCmdline)
	initrd := filepath.Join(driver.Instance.Dir, filenames.Initrd)
	if _, err := os.Stat(baseDisk); errors.Is(err, os.ErrNotExist) {
		var ensuredBaseDisk bool
		errs := make([]error, len(driver.Instance.Config.Images))
		for i, f := range driver.Instance.Config.Images {
			if _, err := fileutils.DownloadFile(ctx, baseDisk, f.File, true, "the image", *driver.Instance.Config.Arch); err != nil {
				errs[i] = err
				continue
			}
			if f.Kernel != nil {
				// ensure decompress kernel because vz expects it to be decompressed
				if _, err := fileutils.DownloadFile(ctx, kernel, f.Kernel.File, true, "the kernel", *driver.Instance.Config.Arch); err != nil {
					errs[i] = err
					continue
				}
				if f.Kernel.Cmdline != "" {
					if err := os.WriteFile(kernelCmdline, []byte(f.Kernel.Cmdline), 0o644); err != nil {
						errs[i] = err
						continue
					}
				}
			}
			if f.Initrd != nil {
				if _, err := fileutils.DownloadFile(ctx, initrd, *f.Initrd, false, "the initrd", *driver.Instance.Config.Arch); err != nil {
					errs[i] = err
					continue
				}
			}
			ensuredBaseDisk = true
			break
		}
		if !ensuredBaseDisk {
			return fileutils.Errors(errs)
		}
	}
	diskSize, _ := units.RAMInBytes(*driver.Instance.Config.Disk)
	if diskSize == 0 {
		return nil
	}
	isBaseDiskISO, err := iso9660util.IsISO9660(baseDisk)
	if err != nil {
		return err
	}
	if isBaseDiskISO {
		// Create an empty data volume (sparse)
		diffDiskF, err := os.Create(diffDisk)
		if err != nil {
			return err
		}

		err = diskUtil.MakeSparse(diffDiskF, 0)
		if err != nil {
			diffDiskF.Close()
			return fmt.Errorf("failed to create sparse diff disk %q: %w", diffDisk, err)
		}
		return diffDiskF.Close()
	}
	if err = diskUtil.ConvertToRaw(baseDisk, diffDisk, &diskSize, false); err != nil {
		return fmt.Errorf("failed to convert %q to a raw disk %q: %w", baseDisk, diffDisk, err)
	}
	return err
}

package taskrunner

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	hclog "github.com/hashicorp/go-hclog"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
)

const (
	// HookNameNix is the name of the Nix hook
	HookNameNix = "nix"
)

// nixHook is used to prepare a task directory structure based on Nix packages
type nixHook struct {
	alloc    *structs.Allocation
	runner   *TaskRunner
	logger   log.Logger
	firstRun bool
}

func newNixHook(runner *TaskRunner, logger log.Logger) *nixHook {
	h := &nixHook{
		alloc:    runner.Alloc(),
		runner:   runner,
		firstRun: true,
	}
	h.logger = logger.Named(h.Name())
	return h
}

func (*nixHook) Name() string {
	return HookNameNix
}

func (h *nixHook) emitEvent(event string, message string) {
	h.runner.EmitEvent(structs.NewTaskEvent(event).SetDisplayMessage(message))
}

func (h *nixHook) emitEventError(event string, err error) {
	h.runner.EmitEvent(structs.NewTaskEvent(event).SetFailsTask().SetSetupError(err))
}

func (h *nixHook) Prestart(ctx context.Context, req *interfaces.TaskPrestartRequest, resp *interfaces.TaskPrestartResponse) error {
	if h.firstRun {
		h.firstRun = false
	} else {
		return nil
	}

	installables := []string{}
	if v, set := req.Task.Config["nix_installables"]; set {
		for _, vv := range v.([]interface{}) {
			installables = append(installables, vv.(string))
		}
	}

	if len(installables) == 0 {
		return nil
	}

	profileInstallArgs := []string{}
	if v, set := req.Task.Config["nix_profile_install_args"]; set {
		profileInstallArgs = v.([]string)
	}

	mount := false
	if v, set := req.Task.Config["nix_host"]; set && v.(bool) {
		mount = true

		resp.Mounts = append(resp.Mounts, &drivers.MountConfig{
			TaskPath:        "/nix",
			HostPath:        "/nix",
			Readonly:        false,
			PropagationMode: "host-to-task",
		})
	}

	return h.install(installables, profileInstallArgs, req.TaskDir.Dir, mount)
}

// install takes an installable like:
// github:NixOS/nixpkgs#cowsay
// github:NixOS/nixpkgs?ref=nixpkgs-unstable#cowsay
// github:NixOS/nixpkgs?rev=04b19784342ac2d32f401b52c38a43a1352cd916#cowsay
// /nix/store/<hash>-<name>
//
// the given installable
func (h *nixHook) install(installables []string, profileInstallArgs []string, taskDir string, mounted bool) error {
	linkPath := filepath.Join(taskDir, "current-alloc")
	_, err := os.Stat(linkPath)
	if err == nil {
		return nil
	}

	h.logger.Debug("Building", "installable", installables)
	h.emitEvent("Nix", "building: "+strings.Join(installables, " "))

	taskDirInfo, err := os.Stat(taskDir)
	if err != nil {
		return err
	}

	uid, gid := getOwner(taskDirInfo)

	for _, installable := range installables {
		if err = h.profileInstall(linkPath, installable, profileInstallArgs); err != nil {
			return err
		}
	}

	if !mounted {
		requisites, err := h.requisites(linkPath)
		if err != nil {
			return err
		}

		// Now copy each dependency into the allocation /nix/store directory
		for _, requisit := range requisites {
			h.logger.Debug("linking", "requisit", requisit)

			err = filepath.Walk(requisit, installAll(h.logger, taskDir, false, false, uid, gid))
			if err != nil {
				return err
			}
		}
	}

	link, err := filepath.EvalSymlinks(linkPath)
	if err != nil {
		return err
	}

	h.logger.Debug("linking main drv paths", "linkPath", linkPath, "link", link)

	return filepath.Walk(link, installAll(h.logger, taskDir, true, mounted, uid, gid))
}

func (h *nixHook) profileInstall(linkPath string, installable string, extraArgs []string) error {
	h.logger.Debug("Building", "installable", installable)
	h.emitEvent("Nix", "building: "+installable)

	args := []string{"profile", "install", "-L", "--no-write-lock-file", "--profile", linkPath}
	args = append(append(args, extraArgs...), installable)
	cmd := exec.Command("nix", args...)
	output, err := cmd.CombinedOutput()

	h.logger.Debug(cmd.String(), "output", string(output))

	if err != nil {
		h.logger.Error(cmd.String(), "output", string(output), "error", err)
		h.emitEvent("Nix", "build failed with error: "+err.Error()+" output: "+string(output))
	}

	return err
}

// Collect all store paths required to run it
func (h *nixHook) requisites(outPath string) ([]string, error) {
	cmd := exec.Command("nix-store", "--query", "--requisites", outPath)
	nixStoreOutput, err := cmd.Output()

	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			h.logger.Error(cmd.String(), "error", err, "stderr", string(ee.Stderr))
		} else {
			h.logger.Error(cmd.String(), "error", err, "stdout", string(nixStoreOutput))
		}
		return []string{}, err
	}

	return strings.Fields(string(nixStoreOutput)), nil
}

func installAll(logger hclog.Logger, targetDir string, truncate, link bool, uid, gid int) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		var dst string
		if truncate {
			parts := splitPath(path)
			dst = filepath.Join(append([]string{targetDir}, parts[3:]...)...)
		} else {
			dst = filepath.Join(targetDir, path)
		}

		// Skip the file if it already exists at the dst
		stat, err := os.Stat(dst)
		lstat, _ := os.Lstat(dst)
		if err == nil {
			return nil
		}
		if !os.IsNotExist(err) {
			logger.Debug("stat errors", "err", err, "stat",
				fmt.Sprintf("%#v", stat),
			)
			return err
		}

		if info.Mode()&os.ModeSymlink != 0 {
			symlink, err := os.Readlink(path)
			if err != nil {
				return err
			}
			// logger.Debug("l", "symlink", symlink, "dst", dst)
			if err := os.Symlink(symlink, dst); err != nil {
				if !os.IsExist(err) {
					logger.Debug("stat", fmt.Sprintf("%#v", stat))
					logger.Debug("lstat", fmt.Sprintf("%#v", lstat))
					return err
				}
			}
			if info.IsDir() {
				return filepath.SkipDir
			} else {
				return nil
			}
		}

		if info.IsDir() {
			// logger.Debug("d", "dst", dst)
			return os.MkdirAll(dst, 0777)
		}

		if link {
			if err := os.Symlink(path, dst); err != nil {
				return fmt.Errorf("Couldn't link %q to %q: %v", path, dst, err)
			}

			if err := os.Lchown(dst, uid, gid); err != nil {
				return fmt.Errorf("Couldn't chown link %q to %q: %v", dst, path, err)
			}
		} else {
			// logger.Debug("f", "dst", dst)
			srcfd, err := os.Open(path)
			if err != nil {
				return err
			}
			defer srcfd.Close()

			dstfd, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE, info.Mode())
			if err != nil {
				return err
			}
			defer dstfd.Close()

			if _, err = io.Copy(dstfd, srcfd); err != nil {
				return err
			}

			if err := dstfd.Chown(uid, gid); err != nil {
				return fmt.Errorf("Couldn't copy %q to %q: %v", path, dst, err)
			}
		}

		return nil
	}
}

func getOwner(fi os.FileInfo) (int, int) {
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1
	}
	return int(stat.Uid), int(stat.Gid)
}

// SplitPath splits a file path into its directories and filename.
func splitPath(path string) []string {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	if dir == "/" {
		return []string{base}
	} else {
		return append(splitPath(dir), base)
	}
}

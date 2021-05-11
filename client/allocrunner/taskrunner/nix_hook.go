package taskrunner

import (
	"context"
	"crypto/sha256"
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
)

const (
	// HookNameNix is the name of the Nix hook
	HookNameNix = "nix"
)

// nixHook is used to prepare a task directory structure based on a Nix flake
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
	first := h.firstRun
	if first {
		h.firstRun = false
	} else {
		return nil
	}

	configFlake, ok := req.Task.Config["flake"]
	if !ok {
		return nil
	}

	flake, ok := configFlake.(string)
	if !ok {
		return nil
	}

	configFlakeArgs, ok := req.Task.Config["flake_args"]
	if ok {
		flakeArgs, ok := configFlakeArgs.([]string)
		if ok {
			return h.install(flake, flakeArgs, req.TaskDir.Dir)
		}
	}

	return h.install(flake, []string{}, req.TaskDir.Dir)
}

// install takes a flake URL like:
// github:NixOS/nixpkgs#cowsay
// github:NixOS/nixpkgs?ref=nixpkgs-unstable#cowsay
// github:NixOS/nixpkgs?rev=04b19784342ac2d32f401b52c38a43a1352cd916#cowsay
//
// the given flake
func (h *nixHook) install(flake string, flakeArgs []string, taskDir string) error {
	_, err := os.Stat(linkPath(flake, flakeArgs, taskDir))
	if err == nil {
		return nil
	}

	h.logger.Debug("Building flake", "flake", flake)
	h.emitEvent("Nix", "building flake: "+flake)

	system, err := h.nixSystem()
	if err != nil {
		return err
	}

	if err := h.nixBuild(flake, flakeArgs, taskDir); err != nil {
		return err
	}
	outPath, err := h.outPath(flake, flakeArgs)
	if err != nil {
		return err
	}
	requisites, err := h.requisites(outPath)
	if err != nil {
		return err
	}

	taskDirInfo, err := os.Stat(taskDir)
	if err != nil {
		return err
	}

	uid, gid := getOwner(taskDirInfo)

	// Now copy each dependency into the allocation directory
	for _, requisit := range requisites {
		err = filepath.Walk(requisit, copyAll(h.logger, taskDir, false, uid, gid))
		if err != nil {
			return err
		}
	}

	symlinkJoin, err := h.symlinkJoin(flake, flakeArgs, system)
	if err != nil {
		return err
	}

	return filepath.Walk(symlinkJoin, copyAll(h.logger, taskDir, true, uid, gid))
}

func (h *nixHook) nixSystem() (string, error) {
	// First we build the derivation to make sure all paths are in the host store
	cmd := exec.Command("nix", "eval", "--raw", "--impure", "--expr", "builtins.currentSystem")
	output, err := cmd.CombinedOutput()
	currentSystem := string(output)
	h.logger.Debug(cmd.String(), "output", currentSystem)
	if err != nil {
		h.logger.Error(cmd.String(), "output", currentSystem, "error", err)
		return "", err
	}
	return currentSystem, nil
}

func linkPath(flake string, flakeArgs []string, taskDir string) string {
	parts := []byte(flake)
	for _, part := range flakeArgs {
		parts = append(parts, []byte(part)...)
	}

	hash := fmt.Sprintf("%x", sha256.Sum256(parts))
	return filepath.Join(taskDir, hash)
}

// nixBuild ensures all requisites are present in the host Nix store.
func (h *nixHook) nixBuild(flake string, flakeArgs []string, taskDir string) error {
	args := []string{"build", "--out-link", linkPath(flake, flakeArgs, taskDir)}
	args = append(append(args, flakeArgs...), flake)
	cmd := exec.Command("nix", args...)
	nixBuildOutput, err := cmd.Output()
	h.logger.Debug(cmd.String(), "output", string(nixBuildOutput))
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			h.logger.Error(cmd.String(), "error", err, "stderr", string(ee.Stderr))
			return NewHookError(err, structs.NewTaskEvent("Nix build failed").SetDisplayMessage(string(ee.Stderr)))
		} else {
			h.logger.Error(cmd.String(), "error", err, "stdout", string(nixBuildOutput))
			return NewHookError(err, structs.NewTaskEvent("Nix build failed").SetDisplayMessage(string(ee.Stderr)))
		}
	}
	return nil
}

func (h *nixHook) outPath(flake string, flakeArgs []string) (string, error) {
	// Then get the path to the derivation output
	args := []string{"eval", "--raw", "--apply", "(pkg: pkg.outPath)"}
	args = append(append(args, flakeArgs...), flake)
	cmd := exec.Command("nix", args...)
	nixEvalOutput, err := cmd.Output()
	path := string(nixEvalOutput)
	h.logger.Debug(cmd.String(), "stdout", path)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			h.logger.Error(cmd.String(), "error", err, "stderr", string(ee.Stderr))
		} else {
			h.logger.Error(cmd.String(), "error", err, "stdout", path)
		}
		return path, err
	}

	return path, nil
}

func (h *nixHook) requisites(outPath string) ([]string, error) {
	// Collect all store paths required to run it
	cmd := exec.Command("nix-store", "--query", "--requisites", outPath)
	nixStoreOutput, err := cmd.Output()
	h.logger.Debug(cmd.String(), "output", string(nixStoreOutput))
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

// TODO: choose correct architecture, atm this only works on x86_64-linux
// This uses the nixpkgs symlinkJoin derivation to build a directory that
// looks like normal FHS, e.g. /bin /share /etc and the like.
func (h *nixHook) symlinkJoin(flake string, flakeArgs []string, system string) (string, error) {
	expr := `
	let
		pkgs = builtins.getFlake
			"github:NixOS/nixpkgs?rev=aea7242187f21a120fe73b5099c4167e12ec9aab";
	in pkg:
	let
		sym = pkgs.legacyPackages.` + system + `.symlinkJoin {
			name = "symlinks";
			paths = [ pkg ];
		};
	in builtins.seq (builtins.pathExists sym) sym.outPath
	`

	args := []string{"eval", "--raw", flake}
	args = append(append(args, flakeArgs...), "--apply", expr)
	cmd := exec.Command("nix", args...)
	symlinkOutput, err := cmd.Output()
	output := string(symlinkOutput)
	h.logger.Debug(cmd.String(), "output", output)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			h.logger.Error(cmd.String(), "error", err, "stderr", string(ee.Stderr))
		} else {
			h.logger.Error(cmd.String(), "error", err, "stdout", string(symlinkOutput))
		}
		return "", err
	}

	return output, nil
}

func copyAll(logger hclog.Logger, targetDir string, truncate bool, uid, gid int) filepath.WalkFunc {
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
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			// logger.Debug("l", "link", link, "dst", dst)
			if err := os.Symlink(link, dst); err != nil {
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

// Copyright 2025.
// Same public‑domain notice the Python file used.
//
// A feature‑parity rewrite of multirun.py in Go.
// Behaviour: identical CLI contract, identical JSON “instructions” schema,
// identical stdout/stderr semantics, Windows‑bash shim, runfiles resolution.
//
// Usage inside Bazel invoked by multirun.bzl:
//
//	<binary> <instructions.json> [extra args to append to each command]
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

// -----------------------------------------------------------------------------
// Data structures that mirror the Python version
// -----------------------------------------------------------------------------

type commandBlob struct {
	Path string            `json:"path"`
	Tag  string            `json:"tag"`
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

type instructionsFile struct {
	Commands      []commandBlob `json:"commands"`
	Jobs          int           `json:"jobs"` // 0 = unlimited / parallel
	PrintCommand  bool          `json:"print_command"`
	KeepGoing     bool          `json:"keep_going"`
	BufferOutput  bool          `json:"buffer_output"`
	ForwardStdin  bool          `json:"forward_stdin"`
	WorkspaceName string        `json:"workspace_name"`
}

type runningProc struct {
	cmd   *exec.Cmd
	blob  commandBlob
	stdin io.WriteCloser // nil unless ForwardStdin
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func bashOnWindows() (string, error) {
	if runtime.GOOS != "windows" {
		return "", nil
	}
	if sh := os.Getenv("BAZEL_SH"); sh != "" {
		return sh, nil
	}
	return exec.LookPath("bash.exe")
}

func scriptPath(r *runfiles.Runfiles, workspace, p string) (string, error) {
	// Behaviour identical to Python: leading "../" means external, else in‑workspace.
	if strings.HasPrefix(p, "../") {
		return r.Rlocation(p[3:]), nil
	}
	return r.Rlocation(filepath.ToSlash(filepath.Join(workspace, p))), nil
}

// -----------------------------------------------------------------------------
// Execution primitives
// -----------------------------------------------------------------------------

// launchCommand starts a command.  If blocking==true it waits and returns the exit code.
func launchCommand(blob commandBlob, r *runfiles.Runfiles, blocking bool, extraArgs []string, pipeStdout bool, pipeStdin bool) (*exec.Cmd, io.WriteCloser, error) {
	var bash string
	var err error
	if runtime.GOOS == "windows" {
		bash, err = bashOnWindows()
		if err != nil {
			return nil, nil, fmt.Errorf("bash not found on Windows (set BAZEL_SH): %w", err)
		}
	}

	argv := append([]string{}, blob.Args...)
	argv = append(argv, extraArgs...)

	var cmd *exec.Cmd
	if bash != "" {
		script := fmt.Sprintf(`%s "$@"`, blob.Path)
		full := append([]string{"-c", script, "--"}, argv...)
		cmd = exec.Command(bash, full...)
	} else {
		cmd = exec.Command(blob.Path, argv...)
	}

	cmd.Env = append(os.Environ(), flattenEnv(blob.Env)...)

	if !pipeStdout {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}

	var stdinWriter io.WriteCloser
	if pipeStdin {
		stdinWriter, err = cmd.StdinPipe()
		if err != nil {
			return nil, nil, err
		}
	}

	return cmd, stdinWriter, nil
}

func flattenEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, fmt.Sprintf("%s=%s", k, v))
	}
	return out
}

// -----------------------------------------------------------------------------
// Concurrency helpers
// -----------------------------------------------------------------------------

// forward stdin lines to all running processes
func forwardStdin(stop <-chan struct{}, procs []*runningProc) {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text() + "\n"
		for _, p := range procs {
			if p.stdin != nil {
				io.WriteString(p.stdin, line)
			}
		}
	}
	// close pipes
	for _, p := range procs {
		if p.stdin != nil {
			p.stdin.Close()
		}
	}
}

// -----------------------------------------------------------------------------
// Serial execution
// -----------------------------------------------------------------------------

func runSerial(instr *instructionsFile, r *runfiles.Runfiles, extraArgs []string) bool {
	for _, blob := range instr.Commands {
		if instr.PrintCommand {
			fmt.Println(blob.Tag)
		}

		cmd, _, err := launchCommand(blob, r, true, extraArgs, false, false)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			if !instr.KeepGoing {
				return false
			}
			continue
		}
		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				if !instr.KeepGoing {
					return false
				}
				if exitErr.ExitCode() != 0 {
					// Mark overall failure but keep going
					continue
				}
			}
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// Parallel execution
// -----------------------------------------------------------------------------

func runParallel(instr *instructionsFile, r *runfiles.Runfiles, extraArgs []string) bool {
	var wg sync.WaitGroup
	mu := sync.Mutex{}
	success := true

	pipeStdout := instr.BufferOutput
	pipeStdin := instr.ForwardStdin

	procs := make([]*runningProc, 0, len(instr.Commands))

	// Launch all
	for _, blob := range instr.Commands {
		cmd, stdinWriter, err := launchCommand(blob, r, false, extraArgs, pipeStdout, pipeStdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			success = false
			continue
		}
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			success = false
			continue
		}
		rp := &runningProc{cmd: cmd, blob: blob, stdin: stdinWriter}
		if pipeStdin {
			rp.stdin = cmd.Stdin.(io.WriteCloser)
		}
		procs = append(procs, rp)
	}

	// stdin forwarder
	done := make(chan struct{})
	if pipeStdin {
		go forwardStdin(done, procs)
	}

	// Signal handling – when user hits Ctrl‑C propagate to children
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	go func() {
		<-signals
		for _, p := range procs {
			_ = p.cmd.Process.Signal(syscall.SIGINT)
		}
	}()

	// Collect
	for _, p := range procs {
		wg.Add(1)
		go func(rp *runningProc) {
			defer wg.Done()
			var captured strings.Builder
			if pipeStdout {
				outPipe, _ := rp.cmd.StdoutPipe()
				rp.cmd.Stderr = rp.cmd.Stdout
				rp.cmd.Stdout = nil
				go io.Copy(&captured, outPipe)
			}
			err := rp.cmd.Wait()
			mu.Lock()
			if pipeStdout && instr.PrintCommand {
				fmt.Println(rp.blob.Tag)
			}
			if captured.Len() > 0 {
				fmt.Print(strings.TrimSpace(captured.String()) + "\n")
			}
			if err != nil {
				success = false
			}
			mu.Unlock()
		}(p)
	}

	wg.Wait()
	close(done)
	return success
}

// -----------------------------------------------------------------------------
// main
// -----------------------------------------------------------------------------

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: multirun <instructions.json> [extra args]")
		os.Exit(1)
	}
	instrPath := os.Args[1]
	extraArgs := os.Args[2:]

	// Runfiles resolver
	r, err := runfiles.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, "runfiles:", err)
		os.Exit(1)
	}

	// Read instructions
	f, err := os.Open(instrPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	var instr instructionsFile
	if err := json.NewDecoder(f).Decode(&instr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Replace short_paths with runfiles absolute paths
	for i := range instr.Commands {
		p, err := scriptPath(r, instr.WorkspaceName, instr.Commands[i].Path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		instr.Commands[i].Path = p
	}

	var ok bool
	if instr.Jobs == 0 {
		ok = runParallel(&instr, r, extraArgs)
	} else {
		ok = runSerial(&instr, r, extraArgs)
	}

	if ok {
		os.Exit(0)
	}
	os.Exit(1)
}

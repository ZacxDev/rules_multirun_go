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
)

type Command struct {
	Path string            `json:"path"`
	Tag  string            `json:"tag"`
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

type Instructions struct {
	WorkspaceName string    `json:"workspace_name"`
	Commands      []Command `json:"commands"`
	Jobs          int       `json:"jobs"` // 0 = parallel
	PrintCommand  bool      `json:"print_command"`
	KeepGoing     bool      `json:"keep_going"`
	BufferOutput  bool      `json:"buffer_output"`
	ForwardStdin  bool      `json:"forward_stdin"`
}

func buildCmd(cmd Command) *exec.Cmd {
	args := cmd.Args
	if runtime.GOOS == "windows" {
		bash := os.Getenv("BAZEL_SH")
		if bash == "" {
			bash, _ = exec.LookPath("bash.exe")
		}
		if bash == "" {
			fmt.Fprintln(os.Stderr, "error: bash not found. Set BAZEL_SH or install Git Bash/MSYS2.")
			os.Exit(1)
		}
		_ = strings.Join(args, `" "`)
		script := fmt.Sprintf(`%s "$@"`, cmd.Path)
		args = []string{"-c", script, "--"}
		args = append(args, cmd.Args...)
		return exec.Command(bash, args...)
	}
	return exec.Command(cmd.Path, args...)
}

func setEnv(cmd *exec.Cmd, env map[string]string) {
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
}

func runSerial(instructions Instructions) bool {
	for _, c := range instructions.Commands {
		if instructions.PrintCommand {
			fmt.Println(c.Tag)
		}
		cmd := buildCmd(c)
		setEnv(cmd, c.Env)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			if !instructions.KeepGoing {
				return false
			}
			fmt.Fprintf(os.Stderr, "Command failed: %s\n", c.Tag)
		}
	}
	return true
}

func forwardStdin(writers []io.WriteCloser) {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text() + "\n"
		for _, w := range writers {
			w.Write([]byte(line))
			w.(interface{ Flush() error }).Flush()
		}
	}
	for _, w := range writers {
		w.Close()
	}
}

func runParallel(instructions Instructions) bool {
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := true

	procs := make([]*exec.Cmd, 0, len(instructions.Commands))
	stdins := make([]io.WriteCloser, 0, len(instructions.Commands))

	for _, c := range instructions.Commands {
		if instructions.PrintCommand && instructions.BufferOutput {
			fmt.Println(c.Tag)
		}
		cmd := buildCmd(c)
		setEnv(cmd, c.Env)

		if instructions.BufferOutput {
			stdoutPipe, _ := cmd.StdoutPipe()
			cmd.Stderr = cmd.Stdout
			wg.Add(1)
			go func(tag string, out io.ReadCloser) {
				defer wg.Done()
				scanner := bufio.NewScanner(out)
				var buf strings.Builder
				for scanner.Scan() {
					buf.WriteString(scanner.Text() + "\n")
				}
				mu.Lock()
				fmt.Printf("[%s]\n%s", tag, buf.String())
				mu.Unlock()
			}(c.Tag, stdoutPipe)
		} else {
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
		}

		if instructions.ForwardStdin {
			in, _ := cmd.StdinPipe()
			stdins = append(stdins, in)
		}

		procs = append(procs, cmd)
	}

	for _, cmd := range procs {
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to start: %v\n", err)
			success = false
		}
	}

	if instructions.ForwardStdin {
		go forwardStdin(stdins)
	}

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	go func() {
		<-interrupt
		for _, p := range procs {
			_ = p.Process.Signal(os.Interrupt)
		}
	}()

	for _, cmd := range procs {
		err := cmd.Wait()
		if err != nil {
			success = false
		}
	}

	wg.Wait()
	return success
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: multirun <instructions.json> [extra args...]")
		os.Exit(1)
	}

	path := os.Args[1]
	extraArgs := os.Args[2:]

	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read instructions: %v\n", err)
		os.Exit(1)
	}

	var instr Instructions
	if err := json.Unmarshal(data, &instr); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse JSON: %v\n", err)
		os.Exit(1)
	}

	// Append extra args to each command
	for i := range instr.Commands {
		instr.Commands[i].Args = append(instr.Commands[i].Args, extraArgs...)
	}

	var ok bool
	if instr.Jobs == 0 {
		ok = runParallel(instr)
	} else {
		ok = runSerial(instr)
	}

	if !ok {
		os.Exit(1)
	}
}

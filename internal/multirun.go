// multirun.go
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
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
	Jobs          int       `json:"jobs"`
	PrintCommand  bool      `json:"print_command"`
	KeepGoing     bool      `json:"keep_going"`
	BufferOutput  bool      `json:"buffer_output"`
	ForwardStdin  bool      `json:"forward_stdin"`
}

func runCommand(cmd Command, block bool, stdin io.Reader, stdout io.Writer) (*exec.Cmd, error) {
	args := cmd.Args
	path := cmd.Path

	if runtime.GOOS == "windows" {
		bash := os.Getenv("BAZEL_SH")
		if bash == "" {
			bash, _ = exec.LookPath("bash.exe")
			if bash == "" {
				return nil, fmt.Errorf("bash not found on Windows")
			}
		}
		args = append([]string{"-c", fmt.Sprintf("%s \"$@\"", cmd.Path), "--"}, args...)
		path = bash
	}

	execCmd := exec.Command(path, args...)
	execCmd.Env = append(os.Environ(), flattenEnv(cmd.Env)...) // merge env
	execCmd.Stdin = stdin
	execCmd.Stdout = stdout
	execCmd.Stderr = stdout

	if block {
		execCmd.Stdout = os.Stdout
		execCmd.Stderr = os.Stderr
		return execCmd, execCmd.Run()
	}

	return execCmd, execCmd.Start()
}

func flattenEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, fmt.Sprintf("%s=%s", k, v))
	}
	return out
}

func performConcurrently(commands []Command, printCommand, bufferOutput, forwardStdin bool, extraArgs []string) bool {
	var wg sync.WaitGroup
	success := true
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	type taggedProc struct {
		Cmd  *exec.Cmd
		Tag  string
		Pipe io.WriteCloser
	}

	procs := make([]taggedProc, 0, len(commands))
	for _, cmd := range commands {
		pr, pw := io.Pipe()
		execCmd, err := runCommand(cmd, false, nil, pw)
		if err != nil {
			log.Printf("failed to start command %s: %v", cmd.Tag, err)
			success = false
			continue
		}

		procs = append(procs, taggedProc{Cmd: execCmd, Tag: cmd.Tag, Pipe: pw})

		if bufferOutput {
			wg.Add(1)
			go func(tag string, r io.Reader) {
				defer wg.Done()
				scanner := bufio.NewScanner(r)
				for scanner.Scan() {
					fmt.Printf("[%s] %s\n", tag, scanner.Text())
				}
			}(cmd.Tag, pr)
		}
	}

	if forwardStdin {
		go func() {
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				line := scanner.Text() + "\n"
				for _, p := range procs {
					p.Pipe.Write([]byte(line))
				}
			}
			for _, p := range procs {
				p.Pipe.Close()
			}
		}()
	}

	for _, proc := range procs {
		if err := proc.Cmd.Wait(); err != nil {
			log.Printf("%s exited with error: %v", proc.Tag, err)
			success = false
		}
	}
	wg.Wait()
	return success
}

func performSerially(commands []Command, printCommand, keepGoing bool) bool {
	success := true
	for _, cmd := range commands {
		if printCommand {
			fmt.Println(cmd.Tag)
		}
		_, err := runCommand(cmd, true, os.Stdin, os.Stdout)
		if err != nil {
			if keepGoing {
				success = false
				continue
			}
			return false
		}
	}
	return success
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: multirun <instructions.json> [extra args...]")
	}

	instructionsPath := os.Args[1]
	extraArgs := os.Args[2:]

	file, err := os.Open(instructionsPath)
	if err != nil {
		log.Fatalf("failed to open instructions: %v", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	var instr Instructions
	if err := decoder.Decode(&instr); err != nil {
		log.Fatalf("failed to parse instructions: %v", err)
	}

	// Adjust paths
	for i := range instr.Commands {
		cmd := &instr.Commands[i]
		if strings.HasPrefix(cmd.Path, "../") {
			cmd.Path = filepath.Join("..", cmd.Path[3:])
		} else {
			cmd.Path = filepath.Join("bazel-bin", instr.WorkspaceName, cmd.Path)
		}
		cmd.Args = append(cmd.Args, extraArgs...)
	}

	var success bool
	if instr.Jobs == 0 {
		success = performConcurrently(instr.Commands, instr.PrintCommand, instr.BufferOutput, instr.ForwardStdin, extraArgs)
	} else {
		success = performSerially(instr.Commands, instr.PrintCommand, instr.KeepGoing)
	}

	if !success {
		os.Exit(1)
	}
}

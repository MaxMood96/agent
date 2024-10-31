// Package shell provides a cross-platform virtual shell abstraction for
// executing commands.
//
// It is intended for internal use by buildkite-agent only.
package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/buildkite/agent/v3/env"
	"github.com/buildkite/agent/v3/internal/olfactor"
	"github.com/buildkite/agent/v3/internal/shellscript"
	"github.com/buildkite/agent/v3/logger"
	"github.com/buildkite/agent/v3/process"
	"github.com/buildkite/agent/v3/tracetools"
	"github.com/buildkite/shellwords"
	"github.com/gofrs/flock"
	"github.com/opentracing/opentracing-go"
)

const lockRetryDuration = time.Second

// ErrShellNotStarted is returned when the shell has not started a process.
var ErrShellNotStarted = errors.New("shell not started")

// Shell represents a virtual shell, handles logging, executing commands and
// provides hooks for capturing output and exit conditions.
//
// Provides a lowest-common denominator abstraction over macOS, Linux and Windows
type Shell struct {
	Logger

	// The running environment for the shell
	Env *env.Environment

	// Whether the shell is a PTY
	PTY bool

	// stdin is an optional input stream used by Run() and friends.
	// It remains unexported on the assumption that it's not useful except via
	// WithStdin() to get a shell-copy prepared for a single command that needs
	// input.
	stdin io.Reader

	// Where stdout is written, defaults to os.Stdout
	Writer io.Writer

	// Whether to run the shell in debug mode
	Debug bool

	// Current working directory that shell commands get executed in
	wd string

	// The currently-running or last-run process.
	proc atomic.Pointer[process.Process]

	// The signal to use to interrupt the command
	InterruptSignal process.Signal

	// Amount of time to wait between sending the InterruptSignal and SIGKILL
	SignalGracePeriod time.Duration

	// How to encode trace contexts.
	traceContextCodec tracetools.Codec
}

type newShellOpt func(*Shell)

func WithLogger(l Logger) newShellOpt {
	return func(s *Shell) {
		s.Logger = l
	}
}

func WithTraceContextCodec(c tracetools.Codec) newShellOpt {
	return func(s *Shell) {
		s.traceContextCodec = c
	}
}

// New returns a new Shell
func New(opts ...newShellOpt) (*Shell, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("Failed to find current working directory: %w", err)
	}

	shell := &Shell{
		Logger:            StderrLogger,
		Env:               env.FromSlice(os.Environ()),
		Writer:            os.Stdout,
		wd:                wd,
		traceContextCodec: tracetools.CodecGob{},
	}

	for _, opt := range opts {
		opt(shell)
	}

	return shell, nil
}

// CloneWithStdin returns a copy of the Shell with the provided [io.Reader] set
// as the Stdin for the next command. The copy should be discarded after one
// command.
// For example:
//
//	sh.CloneWithStdin(strings.NewReader("hello world")).Run("cat")
func (s *Shell) CloneWithStdin(r io.Reader) *Shell {
	// Can't copy struct like `newsh := *s` because atomics can't be copied.
	return &Shell{
		Logger:            s.Logger,
		Env:               s.Env,
		stdin:             r, // our new stdin
		Writer:            s.Writer,
		wd:                s.wd,
		InterruptSignal:   s.InterruptSignal,
		SignalGracePeriod: s.SignalGracePeriod,
		traceContextCodec: s.traceContextCodec,
	}
}

// Getwd returns the current working directory of the shell
func (s *Shell) Getwd() string {
	return s.wd
}

// Chdir changes the working directory of the shell
func (s *Shell) Chdir(path string) error {
	// If the path isn't absolute, prefix it with the current working directory.
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.wd, path)
	}

	s.Promptf("cd %s", shellwords.Quote(path))

	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("Failed to change working: directory does not exist")
	}

	s.wd = path
	return nil
}

// AbsolutePath returns the absolute path to an executable based on the PATH and
// PATHEXT of the Shell
func (s *Shell) AbsolutePath(executable string) (string, error) {
	// Is the path already absolute?
	if path.IsAbs(executable) {
		return executable, nil
	}

	envPath, _ := s.Env.Get("PATH")
	fileExtensions, _ := s.Env.Get("PATHEXT") // For searching .exe, .bat, etc on Windows

	// Use our custom lookPath that takes a specific path
	absolutePath, err := LookPath(executable, envPath, fileExtensions)
	if err != nil {
		return "", err
	}

	// Since the path returned by LookPath is relative to the current working
	// directory, we need to get the absolute version of that.
	return filepath.Abs(absolutePath)
}

// Interrupt interrupts the running process, if there is one.
func (s *Shell) Interrupt() { s.proc.Load().Interrupt() }

// Terminate terminates the running process, if there is one.
func (s *Shell) Terminate() { s.proc.Load().Terminate() }

// Returns the WaitStatus of the shell's process.
//
// The shell must have started at least one process.
func (s *Shell) WaitStatus() (process.WaitStatus, error) {
	p := s.proc.Load()
	if p == nil {
		return nil, ErrShellNotStarted
	}
	return p.WaitStatus(), nil
}

// LockFile is a pid-based lock for cross-process locking
type LockFile interface {
	Unlock() error
}

func (s *Shell) flock(ctx context.Context, path string) (*flock.Flock, error) {
	// + "f" to ensure that flocks and lockfiles never share a filename
	absolutePathToLock, err := filepath.Abs(path + "f")
	if err != nil {
		return nil, fmt.Errorf("failed to find absolute path to lock %q: %w", path, err)
	}

	lock := flock.New(absolutePathToLock)

retryLoop:
	for {
		// Keep trying the lock until we get it
		gotLock, err := lock.TryLock()
		switch {
		case err != nil:
			s.Commentf("Could not acquire lock on %q (%v)", absolutePathToLock, err)
			return nil, err

		case !gotLock:
			s.Commentf("Could not acquire lock on %q (locked by another process)", absolutePathToLock)

		default:
			break retryLoop
		}

		s.Commentf("Trying again in %v...", lockRetryDuration)
		timer := time.NewTimer(lockRetryDuration)
		defer timer.Stop()

		select {
		case <-timer.C:
			// Ready to retry!

		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return lock, nil
}

// LockFile creates a cross-process file-based lock. To set a timeout on
// attempts to acquire the lock, pass a context with a timeout.
func (s *Shell) LockFile(ctx context.Context, path string) (LockFile, error) {
	return s.flock(ctx, path)
}

// Run runs a command, write stdout and stderr to the logger and return an error
// if it fails
func (s *Shell) Run(ctx context.Context, command string, arg ...string) error {
	formatted := process.FormatCommand(command, arg)
	if s.stdin == nil {
		s.Promptf("%s", formatted)
	} else {
		// bash-syntax-compatible indication that input is coming from somewhere
		s.Promptf("%s < /dev/stdin", formatted)
	}

	return s.RunWithoutPrompt(ctx, command, arg...)
}

func (s *Shell) RunWithEnv(ctx context.Context, environ *env.Environment, command string, arg ...string) error {
	formatted := process.FormatCommand(command, arg)
	if s.stdin == nil {
		s.Promptf("%s", formatted)
	} else {
		// bash-syntax-compatible indication that input is coming from somewhere
		s.Promptf("%s < /dev/stdin", formatted)
	}

	cmdCfg, err := s.buildCommand(command, arg...)
	if err != nil {
		s.Errorf("Error building command: %v", err)
		return err
	}

	cmdCfg.Env = append(cmdCfg.Env, environ.ToSlice()...)

	return s.executeCommand(ctx, cmdCfg, s.Writer, executeFlags{
		Stdout: true,
		Stderr: true,
		PTY:    s.PTY,
	})
}

// RunWithOlfactor runs a command, writes stdout and stderr to the shell's writer,
// and returns an error if it fails. If the process exits with a non-zero exit code,
// and `smell` was written to the logger (i.e. the combined stream of stdout and stderr),
// the error will be of type `olfactor.OlfactoryError`. If the process exits 0, the error
// will be nil whether or not the output contained `smell`.
func (s *Shell) RunWithOlfactor(
	ctx context.Context,
	smells []string,
	command string,
	arg ...string,
) (*olfactor.Olfactor, error) {
	formatted := process.FormatCommand(command, arg)
	if s.stdin == nil {
		s.Promptf("%s", formatted)
	} else {
		// bash-syntax-compatible indication that input is coming from somewhere
		s.Promptf("%s < /dev/stdin", formatted)
	}

	cmd, err := s.buildCommand(command, arg...)
	if err != nil {
		s.Errorf("Error building command: %v", err)
		return nil, err
	}

	w, o := olfactor.New(s.Writer, smells)
	return o, s.executeCommand(ctx, cmd, w, executeFlags{
		Stdout: true,
		Stderr: true,
		PTY:    s.PTY,
	})
}

// RunWithoutPrompt runs a command, writes stdout and err to the logger,
// and returns an error if it fails. It doesn't show a prompt.
func (s *Shell) RunWithoutPrompt(ctx context.Context, command string, arg ...string) error {
	cmd, err := s.buildCommand(command, arg...)
	if err != nil {
		s.Errorf("Error building command: %v", err)
		return err
	}

	return s.executeCommand(ctx, cmd, s.Writer, executeFlags{
		Stdout: true,
		Stderr: true,
		PTY:    s.PTY,
	})
}

// RunAndCapture runs a command and captures the output for processing. Stdout is captured, but
// stderr isn't. If the shell is in debug mode then the command will be eched and both stderr
// and stdout will be written to the logger. A PTY is never used for RunAndCapture.
func (s *Shell) RunAndCapture(ctx context.Context, command string, arg ...string) (string, error) {
	if s.Debug {
		s.Promptf("%s", process.FormatCommand(command, arg))
	}

	cmd, err := s.buildCommand(command, arg...)
	if err != nil {
		return "", err
	}

	var b bytes.Buffer

	err = s.executeCommand(ctx, cmd, &b, executeFlags{
		Stdout: true,
		Stderr: false,
		PTY:    false,
	})
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(b.String()), nil
}

// injectTraceCtx adds tracing information to the given env vars to support
// distributed tracing across jobs/builds.
func (s *Shell) injectTraceCtx(ctx context.Context, env *env.Environment) {
	span := opentracing.SpanFromContext(ctx)
	// Not all shell runs will have tracing (nor do they really need to).
	if span == nil {
		return
	}
	if err := tracetools.EncodeTraceContext(span, env.Dump(), s.traceContextCodec); err != nil {
		if s.Debug {
			s.Logger.Warningf("Failed to encode trace context: %v", err)
		}
		return
	}
}

// RunScript is like Run, but the target is an interpreted script which has
// some extra checks to ensure it gets to the correct interpreter. Extra environment vars
// can also be passed the script
func (s *Shell) RunScript(ctx context.Context, path string, extra *env.Environment) error {
	var command string
	var args []string

	// we apply a variety of "feature detection checks" to figure out how we should
	// best run the script

	isSh := filepath.Ext(path) == "" || filepath.Ext(path) == ".sh"
	isWindows := runtime.GOOS == "windows"
	isPwsh := filepath.Ext(path) == ".ps1"

	switch {
	case isWindows && isSh:
		if s.Debug {
			s.Commentf("Attempting to run %s with Bash for Windows", path)
		}
		// Find Bash, either part of Cygwin or MSYS. Must be in the path
		bashPath, err := s.AbsolutePath("bash.exe")
		if err != nil {
			return fmt.Errorf("Error finding bash.exe, needed to run scripts: %v. "+
				"Is Git for Windows installed and correctly in your PATH variable?", err)
		}
		command = bashPath
		args = []string{filepath.ToSlash(path)}

	case isWindows && isPwsh:
		if s.Debug {
			s.Commentf("Attempting to run %s with Powershell", path)
		}
		command = "powershell.exe"
		args = []string{"-file", path}

	case !isWindows && isSh:
		// If the script contains a shebang line, it can be run directly,
		// with the shebang line choosing the interpreter.
		// note that this means that it isn't necessarily a shell script in this case!
		// #!/usr/bin/env python would be totally valid, and would execute as a python script
		sb, err := shellscript.ShebangLine(path)
		if err == nil && sb != "" {
			command = path
			args = nil
			break
		}

		// Bash was the default, so must remain the default.
		shPath, err := s.AbsolutePath("bash")
		if err != nil {
			// It's increasingly popular to not include bash in more minimal
			// container images (e.g. Alpine-based). But because bash has been
			// assumed for so long, many hooks and plugins will be written
			// assuming Bash features.
			// Emit a warning, keep calm, and carry on.
			s.Warningf("Couldn't find bash (%v). Attempting to fall back to sh. This may cause issues for hooks and plugins that assume Bash features.", err)
			shPath, err = s.AbsolutePath("sh")
			if err != nil {
				return fmt.Errorf("error finding a shell, needed to run scripts: %w", err)
			}
		}
		command = shPath
		args = []string{path}

	default:
		// Something else.
		command = path
		args = nil
	}

	cmdCfg, err := s.buildCommand(command, args...)
	if err != nil {
		s.Errorf("Error building command: %v", err)
		return err
	}

	// Combine the two slices of env, let the latter overwrite the former
	environ := env.FromSlice(cmdCfg.Env)
	environ.Merge(extra)
	cmdCfg.Env = environ.ToSlice()

	return s.executeCommand(ctx, cmdCfg, s.Writer, executeFlags{
		Stdout: true,
		Stderr: true,
		PTY:    s.PTY,
	})
}

// buildCommand returns a command that can later be executed.
func (s *Shell) buildCommand(name string, arg ...string) (process.Config, error) {
	// Always use absolute path as Windows has a hard time
	// finding executables in its path
	absPath, err := s.AbsolutePath(name)
	if err != nil {
		return process.Config{}, err
	}

	return process.Config{
		Path:              absPath,
		Args:              arg,
		Env:               append(s.Env.ToSlice(), "PWD="+s.wd),
		Stdin:             s.stdin,
		Dir:               s.wd,
		InterruptSignal:   s.InterruptSignal,
		SignalGracePeriod: s.SignalGracePeriod,
	}, nil
}

type executeFlags struct {
	// Whether to capture stdout
	Stdout bool

	// Whether to capture stderr
	Stderr bool

	// Run the command in a PTY
	PTY bool
}

func round(d time.Duration) time.Duration {
	// The idea here is to show 5 significant digits worth of time.
	// If your build takes 2 hours, you probably don't care about the timing
	// being reported down to the microsecond.
	switch {
	case d < 100*time.Microsecond:
		return d
	case d < time.Millisecond:
		return d.Round(10 * time.Nanosecond)
	case d < 10*time.Millisecond:
		return d.Round(100 * time.Nanosecond)
	case d < 100*time.Millisecond:
		return d.Round(time.Microsecond)
	case d < time.Second:
		return d.Round(10 * time.Microsecond)
	case d < 10*time.Second:
		return d.Round(100 * time.Microsecond)
	case d < time.Minute:
		return d.Round(time.Millisecond)
	case d < 10*time.Minute:
		return d.Round(10 * time.Millisecond)
	case d < time.Hour:
		return d.Round(100 * time.Millisecond)
	default:
		return d.Round(10 * time.Second)
	}
}

func (s *Shell) executeCommand(
	ctx context.Context,
	cmdCfg process.Config,
	w io.Writer,
	flags executeFlags,
) error {
	// Combine the two slices of env, let the latter overwrite the former
	tracedEnv := env.FromSlice(cmdCfg.Env)
	s.injectTraceCtx(ctx, tracedEnv)
	cmdCfg.Env = tracedEnv.ToSlice()

	cmdStr := process.FormatCommand(cmdCfg.Path, cmdCfg.Args)

	if s.Debug {
		t := time.Now()
		defer func() {
			s.Commentf("↳ Command completed in %v", round(time.Since(t)))
		}()
	}

	// Modify process config based on execution flags
	if flags.PTY {
		cmdCfg.PTY = true
		cmdCfg.Stdout = w
	} else {
		// Show stdout if requested or via debug
		if flags.Stdout {
			cmdCfg.Stdout = w
		} else if s.Debug {
			stdOutStreamer := NewLoggerStreamer(s.Logger)
			defer stdOutStreamer.Close()
			cmdCfg.Stdout = stdOutStreamer
		}

		// Show stderr if requested or via debug
		if flags.Stderr {
			cmdCfg.Stderr = w
		} else if s.Debug {
			stdErrStreamer := NewLoggerStreamer(s.Logger)
			defer stdErrStreamer.Close()
			cmdCfg.Stderr = stdErrStreamer
		}
	}

	p := process.New(logger.Discard, cmdCfg)
	s.proc.Store(p)

	if err := p.Run(ctx); err != nil {
		return fmt.Errorf("error running %q: %w", cmdStr, err)
	}

	return p.WaitResult()
}

// ExitCode extracts an exit code from an error where the platform supports it,
// otherwise returns 0 for no error and 1 for an error
func ExitCode(err error) int {
	if err == nil {
		return 0
	}

	if cause := new(ExitError); errors.As(err, &cause) {
		return cause.Code
	}

	if cause := new(exec.ExitError); errors.As(err, &cause) {
		return cause.ExitCode()
	}
	return 1
}

// IsExitSignaled returns true if the error is an ExitError that was
// caused by receiving a signal.
func IsExitSignaled(err error) bool {
	if err == nil {
		return false
	}
	if exitErr := new(exec.ExitError); errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return status.Signaled()
		}
	}
	return false
}

func IsExitError(err error) bool {
	if cause := new(ExitError); errors.As(err, &cause) {
		return true
	}
	if cause := new(exec.ExitError); errors.As(err, &cause) {
		return true
	}
	return false
}

// ExitError is an error that carries a shell exit code
type ExitError struct {
	Code int
	Err  error
}

// Error returns the string message and fulfils the error interface
func (ee *ExitError) Error() string { return ee.Err.Error() }

func (ee *ExitError) Unwrap() error { return ee.Err }

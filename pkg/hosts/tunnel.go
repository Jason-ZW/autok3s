package hosts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/ioutils"
	dockerSig "github.com/docker/docker/pkg/signal"
	"github.com/moby/term"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/terminal"
)

// The default escape key sequence: ctrl-p, ctrl-q
var defaultEscapeKeys = []byte{16, 17}

type Tunnel struct {
	Stdin  io.ReadCloser
	Stdout io.Writer
	Stderr io.Writer
	Writer io.Writer
	Modes  ssh.TerminalModes
	Term   string
	Height int
	Weight int

	DockerExecID   string
	DockerClient   *client.Client
	DockerContext  context.Context
	DockerResponse *types.HijackedResponse

	err  error
	conn *ssh.Client

	cmd *bytes.Buffer
}

func (t *Tunnel) IsDockerTunnel() bool {
	return t.conn == nil && t.DockerClient != nil
}

func (t *Tunnel) Close() error {
	if t.conn != nil {
		if err := t.conn.Close(); err != nil {
			return err
		}
	}
	if t.DockerResponse != nil {
		t.DockerResponse.Close()
	}
	if t.DockerClient != nil {
		if err := t.DockerClient.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tunnel) Cmd(cmd string) *Tunnel {
	if t.cmd == nil {
		t.cmd = bytes.NewBufferString(cmd + "\n")
		return t
	}

	_, err := t.cmd.WriteString(cmd + "\n")
	if err != nil {
		t.err = err
	}

	return t
}

func (t *Tunnel) Terminal() error {
	session, err := t.conn.NewSession()
	defer func() {
		_ = session.Close()
	}()
	if err != nil {
		return err
	}

	termEnv := os.Getenv("TERM")
	if termEnv == "" {
		t.Term = "xterm-256color"
	}
	t.Modes = ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}

	fdInfo, _ := term.GetFdInfo(t.Stdout)
	fd := int(fdInfo)

	oldState, err := terminal.MakeRaw(fd)
	defer func() {
		_ = terminal.Restore(fd, oldState)
	}()
	if err != nil {
		return err
	}

	t.Weight, t.Height, err = terminal.GetSize(fd)
	if err != nil {
		return err
	}

	session.Stdin = os.Stdin
	session.Stdout = os.Stdout
	session.Stderr = os.Stderr

	if err := session.RequestPty(t.Term, t.Height, t.Weight, t.Modes); err != nil {
		return err
	}

	if err := session.Shell(); err != nil {
		return err
	}

	if err := session.Wait(); err != nil {
		return err
	}

	return nil
}

func (t *Tunnel) DockerTerminal() error {
	defer func() {
		if t.DockerResponse != nil {
			t.DockerResponse.Close()
		}
		if t.DockerClient != nil {
			_ = t.DockerClient.Close()
		}
	}()

	stdin, stdout, _ := term.StdStreams()
	t.SetStdio(stdout, nil, stdin)

	if err := t.DockerExecStart(); err != nil {
		return err
	}

	fd, _ := term.GetFdInfo(t.Stdout)
	if term.IsTerminal(fd) {
		if err := t.MonitorDockerTtySize(t.DockerContext, t.DockerExecID); err != nil {
			logrus.Errorf("error monitoring tty size: %s", err.Error())
		}
	}

	if err := t.DockerTerminalWait(); err != nil {
		if !errors.Is(err, io.EOF) {
			return err
		}
	}

	return nil
}

func (t *Tunnel) DockerExecStart() error {
	restoreInput, err := t.SetInput()
	if err != nil {
		return fmt.Errorf("unable to setup input stream: %s", err)
	}

	defer restoreInput()

	errCh := make(chan error, 1)

	go func() {
		defer close(errCh)
		errCh <- func() error {
			outputDone := t.BeginOutputStream(restoreInput)
			inputDone, detached := t.BeginInputStream(restoreInput)

			select {
			case err := <-outputDone:
				return err
			case <-inputDone:
				// input stream has closed.
				if t.Stdout != nil || t.Stderr != nil {
					// wait for output to complete streaming.
					select {
					case err := <-outputDone:
						return err
					case <-t.DockerContext.Done():
						return t.DockerContext.Err()
					}
				}
				return nil
			case err := <-detached:
				// Got a detach key sequence.
				return err
			case <-t.DockerContext.Done():
				return t.DockerContext.Err()
			}
		}()
	}()

	if err := <-errCh; err != nil {
		logrus.Errorf("error hijack: %s", err)
		return err
	}

	return nil
}

func (t *Tunnel) DockerTerminalWait() error {
	resp, err := t.DockerClient.ContainerExecInspect(t.DockerContext, t.DockerExecID)
	if err != nil {
		// if we can't connect, then the daemon probably died.
		if !client.IsErrConnectionFailed(err) {
			return err
		}
		return io.ErrUnexpectedEOF
	}
	status := resp.ExitCode
	if status != 0 {
		return io.EOF
	}
	return nil
}

func (t *Tunnel) SetInput() (restore func(), err error) {
	if t.Stdin == nil {
		// No need to setup input TTY.
		// The restore func is a nop.
		return func() {}, nil
	}

	inFd, _ := term.GetFdInfo(t.Stdin)
	outFd, _ := term.GetFdInfo(t.Stdout)
	inState, _ := term.SetRawTerminal(inFd)
	outState, _ := term.SetRawTerminalOutput(outFd)

	// Use sync.Once so we may call restore multiple times but ensure we
	// only restore the terminal once.
	var restoreOnce sync.Once
	restore = func() {
		restoreOnce.Do(func() {
			if outState != nil {
				_ = term.RestoreTerminal(inFd, outState)
			}
			if inState != nil {
				_ = term.RestoreTerminal(outFd, inState)
			}
		})
	}

	// Wrap the input to detect detach escape sequence.
	// Use default escape keys if an invalid sequence is given.
	escapeKeys := defaultEscapeKeys
	t.Stdin = ioutils.NewReadCloserWrapper(term.NewEscapeProxy(t.Stdin, escapeKeys), t.Stdin.Close)

	return restore, nil
}

func (t *Tunnel) BeginOutputStream(restoreInput func()) <-chan error {
	outputDone := make(chan error)
	go func() {
		var err error

		// when TTY is ON, use regular copy
		_, err = io.Copy(t.Stdout, t.DockerResponse.Reader)
		// we should restore the terminal as soon as possible
		// once the connection ends so any following print
		// messages will be in normal type.

		restoreInput()

		logrus.Debugf("[hijack] end of stdout")

		if err != nil {
			logrus.Errorf("error receive Stdout: %s", err)
		}

		outputDone <- err
	}()

	return outputDone
}

func (t *Tunnel) BeginInputStream(restoreInput func()) (doneC <-chan struct{}, detachedC <-chan error) {
	inputDone := make(chan struct{})
	detached := make(chan error)

	go func() {
		if t.Stdin != nil {
			_, err := io.Copy(t.DockerResponse.Conn, t.Stdin)
			// we should restore the terminal as soon as possible
			// once the connection ends so any following print
			// messages will be in normal type.
			restoreInput()

			logrus.Debug("end of stdin")

			if _, ok := err.(term.EscapeError); ok {
				detached <- err
				return
			}

			if err != nil {
				// this error will also occur on the receive
				// side (from stdout) where it will be
				// propagated back to the caller.
				logrus.Errorf("error send Stdin: %s", err)
			}
		}

		if err := t.DockerResponse.CloseWrite(); err != nil {
			logrus.Errorf("couldn't send EOF: %s", err)
		}

		close(inputDone)
	}()

	return inputDone, detached
}

func (t *Tunnel) ResizeDockerTtyTo(ctx context.Context, execID string, height, width uint) error {
	if height == 0 && width == 0 {
		return nil
	}

	return t.DockerClient.ContainerExecResize(ctx, execID, types.ResizeOptions{
		Height: height,
		Width:  width,
	})
}

func (t *Tunnel) ResizeDockerTty(ctx context.Context, execID string) error {
	fd, _ := term.GetFdInfo(t.Stdout)
	winSize, err := term.GetWinsize(fd)
	if err != nil {
		return err
	}

	return t.ResizeDockerTtyTo(ctx, execID, uint(winSize.Height), uint(winSize.Width))
}

// Borrowed from https://github.com/docker/cli/blob/master/cli/command/container/tty.go#L71.
func (t *Tunnel) MonitorDockerTtySize(ctx context.Context, execID string) error {
	ttyFunc := t.ResizeDockerTty

	if err := ttyFunc(ctx, execID); err != nil {
		go func() {
			var err error
			for retry := 0; retry < 5; retry++ {
				time.Sleep(10 * time.Millisecond)
				if err = ttyFunc(ctx, execID); err == nil {
					break
				}
			}
			if err != nil {
				logrus.Errorf("failed to resize tty, using default size")
			}
		}()
	}

	if runtime.GOOS == "windows" {
		go func() {
			fd, _ := term.GetFdInfo(t.Stdout)
			prevWinSize, err := term.GetWinsize(fd)
			if err != nil {
				return
			}
			prevH := prevWinSize.Height
			prevW := prevWinSize.Width
			for {
				time.Sleep(time.Millisecond * 250)
				winSize, err := term.GetWinsize(fd)
				if err != nil {
					return
				}
				h := winSize.Height
				w := winSize.Width
				if prevW != w || prevH != h {
					_ = t.ResizeDockerTty(ctx, execID)
				}
				prevH = h
				prevW = w
			}
		}()
	} else {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, dockerSig.SIGWINCH)
		go func() {
			for range sigChan {
				_ = t.ResizeDockerTty(ctx, execID)
			}
		}()
	}
	return nil
}

func (t *Tunnel) Run() error {
	if t.err != nil {
		return t.err
	}

	return t.executeCommands()
}

func (t *Tunnel) SetStdio(stdout, stderr io.Writer, stdin io.ReadCloser) *Tunnel {
	if stdout == nil {
		t.Stdout = os.Stdout
	} else {
		t.Stdout = stdout
	}
	if stderr == nil {
		t.Stderr = os.Stderr
	} else {
		t.Stderr = stderr
	}
	if stdin == nil {
		t.Stdin = os.Stdin
	} else {
		t.Stdin = stdin
	}

	return t
}

func (t *Tunnel) executeCommands() error {
	for {
		cmd, err := t.cmd.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		if err := t.executeCommand(cmd); err != nil {
			return err
		}
	}

	return nil
}

func (t *Tunnel) executeCommand(cmd string) error {
	session, err := t.conn.NewSession()
	if err != nil {
		return err
	}

	defer func() {
		_ = session.Close()
	}()

	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	stderrPipe, err := session.StderrPipe()
	if err != nil {
		return err
	}

	var outWriter, errWriter io.Writer
	if t.Writer != nil {
		outWriter = io.MultiWriter(t.Stdout, t.Writer)
		errWriter = io.MultiWriter(t.Stderr, t.Writer)
	} else {
		outWriter = io.MultiWriter(os.Stdout, t.Stdout)
		errWriter = io.MultiWriter(os.Stderr, t.Stderr)
	}

	wg := sync.WaitGroup{}

	wg.Add(1)
	go func() {
		_, _ = io.Copy(outWriter, stdoutPipe)
		wg.Done()
	}()

	wg.Add(1)
	go func() {
		_, _ = io.Copy(errWriter, stderrPipe)
		wg.Done()
	}()

	err = session.Run(cmd)

	wg.Wait()

	return err
}

func (t *Tunnel) Session() (*ssh.Session, error) {
	return t.conn.NewSession()
}

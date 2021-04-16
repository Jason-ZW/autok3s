package hosts

import (
	"bytes"
	"io"
	"os"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/moby/term"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/terminal"
)

type Tunnel struct {
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	Writer   io.Writer
	WsConn   *websocket.Conn
	WsReader *WsReader
	WsWriter *WsWriter
	Modes    ssh.TerminalModes
	Term     string
	Height   int
	Weight   int

	err error

	conn    *ssh.Client
	session *ssh.Session

	cmd *bytes.Buffer
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
	_, err := t.Session()
	if err != nil {
		return err
	}
	defer func() {
		_ = t.Close()
	}()

	if t.isWsTunnel() {
		t.SetStdio(t.WsWriter, t.WsWriter, t.WsReader)
		t.WsReader.SetResizeFunction(t.ChangeWindowSize)
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

	fdInt, _ := term.GetFdInfo(t.Stdin)
	fd := int(fdInt)

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

	t.session.Stdin = t.Stdin
	t.session.Stdout = t.Stdout
	t.session.Stderr = t.Stderr

	if err := t.session.RequestPty(t.Term, t.Height, t.Weight, t.Modes); err != nil {
		return err
	}

	if err := t.session.Shell(); err != nil {
		return err
	}

	if err := t.session.Wait(); err != nil {
		return err
	}

	return nil
}

func (t *Tunnel) ChangeWindowSize(win *WindowSize) {
	if err := t.session.WindowChange(win.Height, win.Width); err != nil {
		logrus.Errorf("[ssh terminal] failed to change ssh window size: %v", err)
	}
}

func (t *Tunnel) Run() error {
	if t.err != nil {
		return t.err
	}

	return t.executeCommands()
}

func (t *Tunnel) SetStdio(stdout, stderr io.Writer, stdin io.ReadCloser) *Tunnel {
	if stdout != nil {
		t.Stdout = stdout
	}
	if stderr != nil {
		t.Stderr = stderr
	}
	if stdin != nil {
		t.Stdin = stdin
	}
	return t
}

func (t *Tunnel) SetSize(height, weight int) {
	t.Height = height
	t.Weight = weight
}

func (t *Tunnel) Close() error {
	if err := t.SessionClose(); err != nil {
		return err
	}
	if t.conn != nil {
		if err := t.conn.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tunnel) Session() (*ssh.Session, error) {
	if t.session == nil {
		session, err := t.conn.NewSession()
		if err != nil {
			return nil, err
		}
		t.session = session
	}
	return t.session, nil
}

func (t *Tunnel) SessionClose() error {
	if t.session != nil {
		return t.session.Close()
	}
	return nil
}

func (t *Tunnel) isWsTunnel() bool {
	return t.WsConn != nil
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

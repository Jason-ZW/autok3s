package hosts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/cnrancher/autok3s/pkg/common"
	"github.com/cnrancher/autok3s/pkg/types"
	"github.com/cnrancher/autok3s/pkg/utils"

	"github.com/moby/term"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/terminal"
	"k8s.io/apimachinery/pkg/util/wait"
)

type SSHDialer struct {
	sshKey          string
	sshCert         string
	sshAddress      string
	username        string
	password        string
	passphrase      string
	useSSHAgentAuth bool

	Stdin  io.ReadCloser
	Stdout io.Writer
	Stderr io.Writer
	Writer io.Writer

	Height int
	Weight int

	Term  string
	Modes ssh.TerminalModes

	ctx     context.Context
	conn    *ssh.Client
	session *ssh.Session
	cmd     *bytes.Buffer

	err error
}

func NewSSHDialer(n *types.Node, timeout bool) (*SSHDialer, error) {
	if len(n.PublicIPAddress) <= 0 && n.InstanceID == "" {
		return nil, errors.New("[ssh-dialer] no node IP or node ID is specified")
	}

	d := &SSHDialer{
		username:        n.SSHUser,
		password:        n.SSHPassword,
		passphrase:      n.SSHKeyPassphrase,
		useSSHAgentAuth: n.SSHAgentAuth,
		sshCert:         n.SSHCert,
		ctx:             context.Background(),
	}

	// IP addresses are preferred.
	if len(n.PublicIPAddress) > 0 {
		d.sshAddress = fmt.Sprintf("%s:%s", n.PublicIPAddress[0], n.SSHPort)
	} else {
		d.sshAddress = n.InstanceID
	}

	// checks and assigns SSH credentials.
	if d.password == "" && d.sshKey == "" && !d.useSSHAgentAuth && len(n.SSHKeyPath) > 0 {
		var err error
		d.sshKey, err = utils.SSHPrivateKeyPath(n.SSHKeyPath)
		if err != nil {
			return nil, err
		}

		if d.sshCert == "" && len(n.SSHCertPath) > 0 {
			d.sshCert, err = utils.SSHCertificatePath(n.SSHCertPath)
			if err != nil {
				return nil, err
			}
		}
	}

	duration := time.Duration((common.Backoff.Steps - 1) * int(common.Backoff.Duration))
	if !timeout {
		duration = 0
	}

	cfg, err := utils.GetSSHConfig(d.username, d.sshKey, d.passphrase, d.sshCert, d.password, duration, d.useSSHAgentAuth)
	if err != nil {
		return nil, err
	}

	if err := wait.ExponentialBackoff(common.Backoff, func() (bool, error) {
		// establish connection with SSH server.
		d.conn, err = ssh.Dial("tcp", d.sshAddress, cfg)
		if err != nil {
			return false, err
		}

		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("[ssh-dialer] establish ssh connect [%s] error: %w", d.sshAddress, err)
	}

	d.session, err = d.conn.NewSession()
	if err != nil {
		return nil, err
	}

	return d, nil
}

// Close close the SSH connection.
func (d *SSHDialer) Close() error {
	if d.session != nil {
		if err := d.session.Close(); err != nil {
			return err
		}
	}
	if d.conn != nil {
		if err := d.conn.Close(); err != nil {
			return err
		}
	}
	return nil
}

// SetStdio set dialer's reader and writer.
func (d *SSHDialer) SetStdio(stdout, stderr io.Writer, stdin io.ReadCloser) *SSHDialer {
	d.Stdout = stdout
	d.Stderr = stderr
	d.Stdin = stdin
	return d
}

// SetDefaultSize set dialer's default win size.
func (d *SSHDialer) SetDefaultSize(height, weight int) *SSHDialer {
	d.Height = height
	d.Weight = weight
	return d
}

// SetWriter set dialer's logs writer.
func (d *SSHDialer) SetWriter(w io.Writer) *SSHDialer {
	d.Writer = w
	return d
}

// Cmd pass commands in dialer, support multiple calls, e.g. d.Cmd("ls").Cmd("id").
func (d *SSHDialer) Cmd(cmd string) *SSHDialer {
	if d.cmd == nil {
		d.cmd = bytes.NewBufferString(cmd + "\n")
		return d
	}

	_, err := d.cmd.WriteString(cmd + "\n")
	if err != nil {
		d.err = err
	}

	return d
}

// Run run commands in remote server via SSH tunnel.
func (d *SSHDialer) Run() error {
	if d.err != nil {
		return d.err
	}

	return d.executeCommands()
}

// Terminal open ssh terminal.
func (d *SSHDialer) Terminal() error {
	defer func() {
		_ = d.session.Close()
	}()

	d.Term = "xterm-256color"
	if os.Getenv("TERM") != "" {
		d.Term = os.Getenv("TERM")
	}
	d.Modes = ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}

	fd, _ := term.GetFdInfo(d.Stderr)
	oldState, err := terminal.MakeRaw(int(fd))
	defer func() {
		_ = terminal.Restore(int(fd), oldState)
	}()
	if err != nil {
		return err
	}

	d.Weight, d.Height, err = terminal.GetSize(int(fd))
	if err != nil {
		return err
	}

	d.session.Stdin = d.Stdin
	d.session.Stdout = d.Stdout
	d.session.Stderr = d.Stderr

	if err := d.session.RequestPty(d.Term, d.Height, d.Weight, d.Modes); err != nil {
		return err
	}

	if err := d.session.Shell(); err != nil {
		return err
	}

	if err := d.session.Wait(); err != nil {
		return err
	}

	return nil
}

func (d *SSHDialer) executeCommands() error {
	for {
		cmd, err := d.cmd.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		if err := d.executeCommand(cmd); err != nil {
			return err
		}
	}

	return nil
}

func (d *SSHDialer) executeCommand(cmd string) error {
	defer func() {
		_ = d.session.Close()
	}()

	stdoutPipe, err := d.session.StdoutPipe()
	if err != nil {
		return err
	}
	stderrPipe, err := d.session.StderrPipe()
	if err != nil {
		return err
	}

	var outWriter, errWriter io.Writer
	if d.Writer != nil {
		outWriter = io.MultiWriter(d.Stdout, d.Writer)
		errWriter = io.MultiWriter(d.Stderr, d.Writer)
	} else {
		outWriter = io.MultiWriter(os.Stdout, d.Stdout)
		errWriter = io.MultiWriter(os.Stderr, d.Stderr)
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

	err = d.session.Run(cmd)

	wg.Wait()

	return err
}

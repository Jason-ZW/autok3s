package hosts

import (
	"errors"
	"fmt"
	"time"

	"github.com/cnrancher/autok3s/pkg/common"
	"github.com/cnrancher/autok3s/pkg/types"
	"github.com/cnrancher/autok3s/pkg/utils"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	tcpNetProtocol  = "tcp"
	wsProtocol      = "websocket"
	kubectlProtocol = "kubectl"
	wsKind          = "websocket"
	kubectlKind     = "kubectl"
	sshKind         = "ssh"
)

type Host struct {
	types.Node `json:",inline"`
}

type Dialer struct {
	sshKey     string
	sshCert    string
	sshAddress string
	username   string
	password   string
	passphrase string
	netConn    string

	useSSHAgentAuth bool
}

func SSHDialer(h *Host) (*Dialer, error) {
	return newDialer(h, sshKind)
}

func WsDialer(h *Host) (*Dialer, error) {
	return newDialer(h, wsKind)
}

func KubectlDialer(h *Host) (*Dialer, error) {
	return newDialer(h, kubectlKind)
}

func (d *Dialer) OpenTunnel(timeout bool, wsConn *websocket.Conn) (*Tunnel, error) {
	var conn *ssh.Client
	var wsReader *WsReader
	var wsWriter *WsWriter
	var err error

	if err := wait.ExponentialBackoff(common.Backoff, func() (bool, error) {
		switch d.netConn {
		case tcpNetProtocol:
			conn, err = d.getSSHTunnelConnection(timeout)
		case wsProtocol, kubectlProtocol:
			wsReader, wsWriter = d.getWsTunnelConnection(wsConn)
		}
		if err != nil {
			return false, nil
		}
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("[dialer] calling openTunnel [%s] error: %w", d.sshAddress, err)
	}

	return &Tunnel{conn: conn, WsConn: wsConn, WsReader: wsReader, WsWriter: wsWriter}, nil
}

func newDialer(h *Host, kind string) (*Dialer, error) {
	d := &Dialer{}

	if len(h.PublicIPAddress) <= 0 && h.InstanceID == "" {
		return nil, errors.New("[dialer] no node IP or node ID is specified")
	}

	if len(h.PublicIPAddress) > 0 {
		d.sshAddress = fmt.Sprintf("%s:%s", h.PublicIPAddress[0], h.SSHPort)
		d.username = h.SSHUser
		d.password = h.SSHPassword
		d.passphrase = h.SSHKeyPassphrase
		d.useSSHAgentAuth = h.SSHAgentAuth
		d.sshCert = h.SSHCert
		if d.password == "" && d.sshKey == "" && !d.useSSHAgentAuth && len(h.SSHKeyPath) > 0 {
			var err error
			d.sshKey, err = utils.SSHPrivateKeyPath(h.SSHKeyPath)
			if err != nil {
				return nil, err
			}

			if d.sshCert == "" && len(h.SSHCertPath) > 0 {
				d.sshCert, err = utils.SSHCertificatePath(h.SSHCertPath)
				if err != nil {
					return nil, err
				}
			}
		}
	} else {
		d.sshAddress = h.InstanceID
	}

	switch kind {
	case sshKind:
		d.netConn = tcpNetProtocol
	case wsKind:
		d.netConn = wsProtocol
	case kubectlKind:
		d.netConn = kubectlProtocol
	}

	return d, nil
}

func (d *Dialer) getSSHTunnelConnection(t bool) (*ssh.Client, error) {
	timeout := time.Duration((common.Backoff.Steps - 1) * int(common.Backoff.Duration))
	if !t {
		timeout = 0
	}

	cfg, err := utils.GetSSHConfig(d.username, d.sshKey, d.passphrase, d.sshCert, d.password, timeout, d.useSSHAgentAuth)
	if err != nil {
		return nil, err
	}
	// establish connection with SSH server.
	return ssh.Dial(tcpNetProtocol, d.sshAddress, cfg)
}

func (d *Dialer) getWsTunnelConnection(conn *websocket.Conn) (*WsReader, *WsWriter) {
	return NewWsReader(conn), NewWsWriter(conn)
}

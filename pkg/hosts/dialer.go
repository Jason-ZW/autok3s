package hosts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cnrancher/autok3s/pkg/common"
	"github.com/cnrancher/autok3s/pkg/types"
	"github.com/cnrancher/autok3s/pkg/utils"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"
	"github.com/gorilla/websocket"
	dockerutils "github.com/rancher/k3d/v4/pkg/runtimes/docker"
	k3d "github.com/rancher/k3d/v4/pkg/types"
	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	sshKind     = "ssh"
	tcpProtocol = "tcp"

	wsKind     = "websocket"
	wsProtocol = "websocket"

	kubectlKind     = "kubectl"
	kubectlProtocol = "kubectl"

	dockerKind     = "docker"
	dockerProtocol = "docker"
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
	container  string

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

func DockerDialer(h *Host) (*Dialer, error) {
	return newDialer(h, dockerKind)
}

func (d *Dialer) OpenTunnel(ctx context.Context, timeout bool, wsConn *websocket.Conn, container string) (*Tunnel, error) {
	var (
		conn *ssh.Client

		wsReader *WsReader
		wsWriter *WsWriter

		execID   string
		client   *dockerclient.Client
		response *dockertypes.HijackedResponse

		err error
	)

	if err := wait.ExponentialBackoff(common.Backoff, func() (bool, error) {
		switch d.netConn {
		case tcpProtocol:
			conn, err = d.getSSHTunnelConnection(timeout)
		case wsProtocol, kubectlProtocol:
			wsReader, wsWriter = d.getWsTunnelConnection(wsConn)
		case dockerProtocol:
			client, execID, response, err = d.getDockerTunnelConnection(container, ctx)
		}
		if err != nil {
			return false, nil
		}
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("[dialer] calling openTunnel [%s] error: %w", d.sshAddress, err)
	}

	return &Tunnel{conn: conn, WsConn: wsConn, WsReader: wsReader, WsWriter: wsWriter, DockerResponse: response, DockerClient: client, DockerExecID: execID, DockerContext: ctx}, nil
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
		if kind == dockerKind {
			d.container = h.InstanceID
		} else {
			d.sshAddress = h.InstanceID
		}
	}

	switch kind {
	case sshKind:
		d.netConn = tcpProtocol
	case wsKind:
		d.netConn = wsProtocol
	case kubectlKind:
		d.netConn = kubectlProtocol
	case dockerKind:
		d.netConn = dockerProtocol
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
	return ssh.Dial(tcpProtocol, d.sshAddress, cfg)
}

func (d *Dialer) getWsTunnelConnection(conn *websocket.Conn) (*WsReader, *WsWriter) {
	return NewWsReader(conn), NewWsWriter(conn)
}

func (d *Dialer) getDockerTunnelConnection(id string, ctx context.Context) (*dockerclient.Client, string, *dockertypes.HijackedResponse, error) {
	// create docker client.
	docker, err := dockerutils.GetDockerClient()
	if err != nil {
		return docker, "", nil, fmt.Errorf("[dialer] failed to get docker client: %w", err)
	}

	// (1) list containers which have the default k3d labels attached.
	f := filters.NewArgs()

	// regex filtering for exact name match.
	// Assumptions:
	// -> container names start with a / (see https://github.com/moby/moby/issues/29997).
	// -> user input may or may not have the "k3d-" prefix.
	f.Add("name", fmt.Sprintf("^/?(%s-)?%s$", k3d.DefaultObjectNamePrefix, id))

	containers, err := docker.ContainerList(ctx, dockertypes.ContainerListOptions{
		Filters: f,
		All:     true,
	})
	if err != nil {
		return nil, "", nil, fmt.Errorf("[dialer] failed to list containers: %+v", err)
	}

	if len(containers) > 1 {
		return nil, "", nil, fmt.Errorf("[dialer] failed to get a single container for name '%s'. found: %d", id, len(containers))
	}

	if len(containers) == 0 {
		return nil, "", nil, fmt.Errorf("[dialer] didn't find container for node '%s'", id)
	}

	container := containers[0]

	// create docker container exec.
	exec, err := docker.ContainerExecCreate(ctx, container.ID, dockertypes.ExecConfig{
		Privileged:   true,
		Tty:          true,
		AttachStdin:  true,
		AttachStderr: true,
		AttachStdout: true,
		Cmd:          []string{"/bin/sh"},
	})
	if err != nil {
		return docker, "", nil, fmt.Errorf("[dialer] failed to create exec config for node '%s': %+v", d.container, err)
	}

	execConnection, err := docker.ContainerExecAttach(ctx, exec.ID, dockertypes.ExecStartCheck{
		Tty: true,
	})
	if err != nil {
		return docker, "", nil, fmt.Errorf("[dialer] failed to connect to exec process in node '%s': %w", d.container, err)
	}

	// establish connection with Docker.
	return docker, exec.ID, &execConnection, nil
}

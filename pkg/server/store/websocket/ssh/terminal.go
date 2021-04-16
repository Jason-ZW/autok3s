package ssh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/cnrancher/autok3s/pkg/common"
	"github.com/cnrancher/autok3s/pkg/hosts"
	websocketutils "github.com/cnrancher/autok3s/pkg/server/store/websocket/utils"
	autok3stypes "github.com/cnrancher/autok3s/pkg/types"

	"github.com/gorilla/websocket"
	"github.com/rancher/apiserver/pkg/apierror"
	"github.com/rancher/wrangler/pkg/schemas/validation"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

type Terminal struct {
	conn         *websocket.Conn
	tunnel       *hosts.Tunnel
	session      *ssh.Session
	sshStdinPipe io.WriteCloser
	reader       *websocketutils.TerminalReader
}

func NewTerminal(conn *websocket.Conn) *Terminal {
	return &Terminal{
		conn: conn,
	}
}

func NewSSHClient(id, node string) (*hosts.Tunnel, error) {
	return getTunnel(id, node)
}

func (t *Terminal) StartTerminal(tunnel *hosts.Tunnel, rows, cols int) error {
	r := websocketutils.NewReader(t.conn)
	t.tunnel = tunnel
	t.reader = r

	if !tunnel.IsDockerTunnel() {
		s, err := tunnel.Session()
		if err != nil {
			return err
		}
		t.session = s

		r.SetResizeFunction(t.ChangeWindowSize)

		t.session.Stdin = r
		w := websocketutils.NewWriter(t.conn)
		t.session.Stdout = w
		t.session.Stderr = w

		term := "xterm"
		modes := ssh.TerminalModes{
			ssh.ECHO:          1,
			ssh.TTY_OP_ISPEED: 14400,
			ssh.TTY_OP_OSPEED: 14400,
		}

		if err = t.session.RequestPty(term, rows, cols, modes); err != nil {
			return err
		}

		if err = t.session.Shell(); err != nil {
			return err
		}
	} else {
		r.SetResizeFunction(t.ChangeWindowSize)

		w := websocketutils.NewWriter(t.conn)

		tunnel.SetStdio(w, nil, r)

		if err := tunnel.DockerExecStart(); err != nil {
			return err
		}
	}

	return nil
}

func (t *Terminal) Close() {
	if t.tunnel != nil {
		_ = t.tunnel.Close()
	}
	if t.session != nil {
		_ = t.session.Close()
	}
}

func (t *Terminal) ChangeWindowSize(win *websocketutils.WindowSize) {
	if t.session != nil {
		if err := t.session.WindowChange(win.Height, win.Width); err != nil {
			logrus.Errorf("[ssh terminal] failed to change ssh window size: %v", err)
		}
	} else if t.tunnel.DockerClient != nil {
		if err := t.tunnel.ResizeDockerTtyTo(t.tunnel.DockerContext, t.tunnel.DockerExecID, uint(win.Height), uint(win.Width)); err != nil {
			logrus.Errorf("[docker terminal] failed to change docker window size: %v", err)
		}
	}
}

func (t *Terminal) ReadMessage(ctx context.Context) error {
	if t.session != nil {
		return websocketutils.ReadMessage(ctx, t.conn, t.Close, t.session.Wait, t.reader.ClosedCh)
	} else if t.tunnel.DockerClient != nil && t.tunnel != nil {
		return websocketutils.ReadMessage(ctx, t.conn, t.Close, t.tunnel.DockerTerminalWait, t.reader.ClosedCh)
	}
	return nil
}

func getTunnel(id, node string) (*hosts.Tunnel, error) {
	// get exist cluster's state from database.
	state, err := common.DefaultDB.GetClusterByID(id)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, fmt.Errorf("cluster %s is not exist", id)
	}

	// aggregate exist cluster's nodes.
	allNodes := make([]autok3stypes.Node, 0)
	err = json.Unmarshal(state.MasterNodes, &allNodes)
	if err != nil {
		return nil, err
	}
	nodes := make([]autok3stypes.Node, 0)
	err = json.Unmarshal(state.WorkerNodes, &nodes)
	if err != nil {
		return nil, err
	}
	allNodes = append(allNodes, nodes...)

	var dialer *hosts.Dialer

	// find the matching node and return to the tunnel.
	for _, n := range allNodes {
		if n.InstanceID == node {
			if state.Provider == "k3d" {
				dialer, err = hosts.DockerDialer(&hosts.Host{Node: n})
			} else {
				dialer, err = hosts.SSHDialer(&hosts.Host{Node: n})
			}
			if err != nil {
				return nil, err
			}
			return dialer.OpenTunnel(true, n.InstanceID, context.Background())
		}
	}
	return nil, apierror.NewAPIError(validation.NotFound, fmt.Sprintf("node %s is not found for cluster [%s]", node, id))
}

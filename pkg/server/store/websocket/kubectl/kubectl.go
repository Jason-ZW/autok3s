// +build darwin linux

package kubectl

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/cnrancher/autok3s/pkg/hosts"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/rancher/apiserver/pkg/apierror"
	"github.com/rancher/apiserver/pkg/types"
	"github.com/rancher/wrangler/pkg/schemas/validation"
	"github.com/sirupsen/logrus"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:    10240,
	WriteBufferSize:   10240,
	HandshakeTimeout:  60 * time.Second,
	EnableCompression: true,
}

type Shell struct {
	conn *websocket.Conn
	ptmx *os.File
}

func KubeHandler(apiOp *types.APIRequest) (types.APIObject, error) {
	err := ptyHandler(apiOp)
	if err != nil {
		logrus.Errorf("error during kubectl handler %v", err)
	}
	return types.APIObject{}, validation.ErrComplete
}

func ptyHandler(apiOp *types.APIRequest) error {
	queryParams := apiOp.Request.URL.Query()
	height := queryParams.Get("height")
	width := queryParams.Get("width")
	rows := 150
	columns := 300
	var err error
	if height != "" {
		rows, err = strconv.Atoi(height)
		if err != nil {
			return apierror.NewAPIError(validation.InvalidOption, fmt.Sprintf("invalid height %s", height))
		}
	}
	if width != "" {
		columns, err = strconv.Atoi(width)
		if err != nil {
			return apierror.NewAPIError(validation.InvalidOption, fmt.Sprintf("invalid width %s", width))
		}
	}

	upgrader.CheckOrigin = func(r *http.Request) bool {
		return true
	}
	c, err := upgrader.Upgrade(apiOp.Response, apiOp.Request, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = c.Close()
	}()

	s := &Shell{
		conn: c,
	}
	return s.startTerminal(apiOp.Request.Context(), rows, columns, apiOp.Name)
}

func (s *Shell) startTerminal(ctx context.Context, rows, cols int, id string) error {
	kubeBash := exec.CommandContext(ctx, "bash")
	// Start the command with a pty.
	p, err := pty.StartWithSize(kubeBash, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
	if err != nil {
		return err
	}
	s.ptmx = p

	dialer, err := hosts.KubectlDialer(&hosts.Host{})
	if err != nil {
		return err
	}

	tunnel, err := dialer.OpenTunnel(context.Background(), false, s.conn, "")
	if err != nil {
		return err
	}

	tunnel.WsReader.SetResizeFunction(s.ChangeSize)

	aliasCmd := fmt.Sprintf("alias kubectl='kubectl --context %s'\n", id)
	aliasCmd = fmt.Sprintf("%salias k='kubectl --context %s'\n", aliasCmd, id)

	_, _ = s.ptmx.Write([]byte(aliasCmd))
	go func() {
		_, _ = io.Copy(s.ptmx, tunnel.WsReader)
	}()
	go func() {
		_, _ = io.Copy(tunnel.WsWriter, s.ptmx)
	}()

	return hosts.ReadMessage(ctx, s.conn, s.Close, kubeBash.Wait, tunnel.WsReader.ClosedCh)
}

func (s *Shell) Close() error {
	if s.ptmx != nil {
		if err := s.ptmx.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Shell) ChangeSize(win *hosts.WindowSize) {
	_ = pty.Setsize(s.ptmx, &pty.Winsize{
		Rows: uint16(win.Height),
		Cols: uint16(win.Width),
	})
}

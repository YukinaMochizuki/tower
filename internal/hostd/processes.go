package hostd

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/YukinaMochizuki/tower/internal/provider"
)

// errNoProcesses answers the process operations until hostd runs them.
var errNoProcesses = connect.NewError(connect.CodeUnimplemented, errors.New("this hostd runs no processes"))

func (h *Host) StartProcess(context.Context, provider.ProcessRequest) (string, error) {
	return "", errNoProcesses
}

func (h *Host) AttachProcess(context.Context, provider.Instance, string, int64, bool, func(provider.OutputChunk) error) (provider.Attached, error) {
	return provider.Attached{}, errNoProcesses
}

func (h *Host) SignalProcess(context.Context, provider.Instance, string, provider.Signal) error {
	return errNoProcesses
}

func (h *Host) ListProcesses(context.Context, provider.Instance) ([]provider.Process, error) {
	return nil, errNoProcesses
}

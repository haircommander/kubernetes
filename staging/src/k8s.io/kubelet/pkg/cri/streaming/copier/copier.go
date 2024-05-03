package copier

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/util/httpstream/wsstream"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/endpoints/responsewriter"
)

// TODO FIXME
const (
	v4BinaryWebsocketProtocol = "v4." + wsstream.ChannelWebSocketProtocol
	v4Base64WebsocketProtocol = "v4." + wsstream.Base64ChannelWebSocketProtocol
)

// Copier knows how to copy files content from a data stream to/from a pod.
type Copier interface {
	CopyToContainer(ctx context.Context, container string, size uint64, checksum uint32, out io.WriteCloser) error
	CopyFromContainer(ctx context.Context, container string, path string, in io.Reader) error
}

// ServeCopyTo handles requests to copy a file to a container.
// It delegates the actual execution to the executor.
func ServeCopyTo(w http.ResponseWriter, req *http.Request, copier Copier, container string, size uint64, checksum uint32, idleTimeout time.Duration) {
	var err error
	if !wsstream.IsWebSocketRequest(req) {
		runtime.HandleError(fmt.Errorf("websocket required for copy"))
	}

	if err = handleCopyTo(w, req, copier, container, size, checksum, idleTimeout); err != nil {
		runtime.HandleError(err)
		return
	}
}

func handleCopyTo(w http.ResponseWriter, req *http.Request, copier Copier, container string, size uint64, checksum uint32, idleTimeout time.Duration) error {
	// TODO FIXME
	channels := []wsstream.ChannelType{wsstream.ReadWriteChannel}

	conn := wsstream.NewConn(map[string]wsstream.ChannelProtocolConfig{
		"": {
			Binary:   true,
			Channels: channels,
		},
		v4BinaryWebsocketProtocol: {
			Binary:   true,
			Channels: channels,
		},
		v4Base64WebsocketProtocol: {
			Binary:   false,
			Channels: channels,
		},
	})
	conn.SetIdleTimeout(idleTimeout)
	_, streams, err := conn.Open(responsewriter.GetOriginal(w), req)
	if err != nil {
		err = fmt.Errorf("unable to upgrade websocket connection: %v", err)
		return err
	}
	defer conn.Close()
	return copier.CopyToContainer(context.Background(), container, size, checksum, streams[0])
}

// ServeCopyFrom handles requests to copy a file to a container.
// It delegates the actual execution to the executor.
func ServeCopyFrom(w http.ResponseWriter, req *http.Request, copier Copier, container, path string, idleTimeout time.Duration) {
	var err error
	if !wsstream.IsWebSocketRequest(req) {
		runtime.HandleError(fmt.Errorf("websocket required for copy"))
	}

	if err = handleCopyFrom(w, req, copier, container, path, idleTimeout); err != nil {
		runtime.HandleError(err)
		return
	}
}

func handleCopyFrom(w http.ResponseWriter, req *http.Request, copier Copier, container, path string, idleTimeout time.Duration) error {
	// TODO FIXME
	channels := []wsstream.ChannelType{wsstream.ReadWriteChannel}
	conn := wsstream.NewConn(map[string]wsstream.ChannelProtocolConfig{
		"": {
			Binary:   true,
			Channels: channels,
		},
		v4BinaryWebsocketProtocol: {
			Binary:   true,
			Channels: channels,
		},
		v4Base64WebsocketProtocol: {
			Binary:   false,
			Channels: channels,
		},
	})
	conn.SetIdleTimeout(idleTimeout)
	_, streams, err := conn.Open(responsewriter.GetOriginal(w), req)
	if err != nil {
		err = fmt.Errorf("unable to upgrade websocket connection: %v", err)
		return err
	}
	defer conn.Close()
	return copier.CopyFromContainer(context.Background(), container, path, streams[0])
}

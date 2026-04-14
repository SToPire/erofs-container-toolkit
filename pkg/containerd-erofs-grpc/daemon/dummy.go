package daemon

import "context"

type DummyDaemonClient struct{}

func NewDummyDaemonClient() *DummyDaemonClient {
	return &DummyDaemonClient{}
}

func (c *DummyDaemonClient) Start(context.Context, DaemonConfig) error {
	return nil
}

func (c *DummyDaemonClient) Stop(context.Context) error {
	return nil
}

func (c *DummyDaemonClient) BindLayer(context.Context, string, RemoteLayerConfig, string) error {
	return nil
}

func (c *DummyDaemonClient) UnbindLayer(context.Context, string) error {
	return nil
}

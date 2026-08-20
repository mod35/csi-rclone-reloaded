package rclone

import (
	"k8s.io/klog/v2"
)

type driver struct {
	name     string
	version  string
	nodeID   string
	endpoint string
}

var (
	DriverName    = "csi-rclone"
	DriverVersion = "latest"
)

func NewDriver(nodeID, endpoint string) *driver {
	klog.Infof("Starting new %s driver in version %s", DriverName, DriverVersion)

	return &driver{
		name:     DriverName,
		version:  DriverVersion,
		nodeID:   nodeID,
		endpoint: endpoint,
	}
}

func NewNodeServer(d *driver) *nodeServer {
	return &nodeServer{nodeID: d.nodeID}
}

func (d *driver) Run() {
	s := newNonBlockingGRPCServer()
	s.Start(d.endpoint,
		&identityServer{name: d.name, version: d.version},
		&controllerServer{},
		NewNodeServer(d))
	s.Wait()
}

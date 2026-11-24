package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
)

// The background service always runs `service run` (Mode up). On a data
// folder that is not in a cluster yet it must not create one on its own:
// that would make an empty, separate cluster on a machine that was meant to
// join. So it waits for a request that `up`, `join` or `witness` leave in
// the data folder (ServiceRequestFile) and then does exactly what that
// command would have done in the foreground.

// ServiceRequestFile is the request's name inside the data folder.
const ServiceRequestFile = "service-request.json"

// ServiceRequest is what `up` / `join` / `witness` hand to the service.
// It may hold an invite code (a secret): the file is 0600 in the private
// data folder and is removed as soon as it is read.
type ServiceRequest struct {
	Mode        string   `json:"mode"` // "up", "join" or "witness"
	Invite      string   `json:"invite,omitempty"`
	Name        string   `json:"name,omitempty"`
	Listen      string   `json:"listen,omitempty"`
	Advertise   []string `json:"advertise,omitempty"`
	PGPort      int      `json:"pg_port,omitempty"`
	ClientPort  int      `json:"client_port,omitempty"`
	ControlPort int      `json:"control_port,omitempty"`
	// AllowMixedOS: `join --allow-mixed-os` (D37).
	AllowMixedOS bool `json:"allow_mixed_os,omitempty"`
}

// ServiceRequestPath is where the request goes for a data folder.
func ServiceRequestPath(dataDir string) string { return filepath.Join(dataDir, ServiceRequestFile) }

// awaitServiceRequest returns once the data folder is in a cluster (or
// half-way through joining one), or a request was read and applied to
// a.cfg.
func (a *Agent) awaitServiceRequest(ctx context.Context) error {
	logged := false
	for {
		has, err := cluster.HasState(a.clusterDir())
		if err != nil {
			return err
		}
		pending, err := cluster.HasPendingJoin(a.clusterDir())
		if err != nil {
			return err
		}
		if has || pending {
			return nil
		}
		req, err := readServiceRequest(a.node.StateDir)
		switch {
		case err == nil:
			a.applyServiceRequest(req)
			return nil
		case !errors.Is(err, fs.ErrNotExist):
			a.log.Warn("unusable service request", "err", err)
			a.say("Could not read the request in %s: %v", ServiceRequestPath(a.node.StateDir), err)
		}
		if !logged {
			logged = true
			a.log.Info("waiting for a request", "file", ServiceRequestPath(a.node.StateDir))
			a.say("This machine is not in a cluster yet. Waiting for `%s up` (new cluster) or `%s join` (it asks for the invite code).", brand.Command, brand.Command)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// ServiceModes maps ServiceRequest.Mode to the agent's modes.
var ServiceModes = map[string]Mode{"up": ModeUp, "join": ModeJoin, "witness": ModeWitness}

func (a *Agent) applyServiceRequest(r ServiceRequest) {
	a.cfg.Mode, a.cfg.Invite = ServiceModes[r.Mode], r.Invite
	a.cfg.AllowMixedOS = r.AllowMixedOS
	if r.Name != "" {
		a.cfg.Name = r.Name
	}
	if r.Listen != "" {
		a.cfg.Listen = r.Listen
	}
	if len(r.Advertise) > 0 {
		a.cfg.Advertise = r.Advertise
	}
	if r.PGPort != 0 {
		a.cfg.PGPort = r.PGPort
	}
	if r.ClientPort != 0 {
		a.cfg.ClientPort = r.ClientPort
	}
	if r.ControlPort != 0 {
		a.cfg.ControlPort = r.ControlPort
	}
	a.log.Info("service request accepted", "mode", r.Mode)
}

// readServiceRequest reads and removes the request (one use only).
func readServiceRequest(dir string) (ServiceRequest, error) {
	p := ServiceRequestPath(dir)
	b, err := os.ReadFile(p)
	if err != nil {
		return ServiceRequest{}, err
	}
	if err := os.Remove(p); err != nil {
		return ServiceRequest{}, fmt.Errorf("remove %s: %w", p, err)
	}
	var r ServiceRequest
	if err := json.Unmarshal(b, &r); err != nil {
		return ServiceRequest{}, fmt.Errorf("parse: %w", err)
	}
	switch r.Mode {
	case "up":
	case "join", "witness":
		if r.Invite == "" {
			return ServiceRequest{}, errors.New("a join request needs an invite code")
		}
	default:
		return ServiceRequest{}, fmt.Errorf("unknown mode %q", r.Mode)
	}
	return r, nil
}

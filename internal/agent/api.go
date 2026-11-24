package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/control"
)

// Local control API (127.0.0.1 only, bearer token from the data dir).
//
//	GET    /v1/status              Status (cluster, members, replication, warnings, problems)
//	GET    /v1/problem-codes       every problem code with its texts (D66; dashboard route, bearer too)
//	POST   /v1/invites             {"ttl":"1h"} → {"code":"...","expires":"..."}
//	POST   /v1/leave               this node leaves the cluster; the agent stops
//	DELETE /v1/members/{node}      remove another member (name or id)
//	POST   /v1/members/{node}/addresses  {"addresses":[...],"force":false} → SetAddressResponse
//	POST   /v1/switch              {"to":"name"} → SwitchResult (planned switch)
//	GET    /v1/settings            cluster settings
//	POST   /v1/settings            {"set":{"key":"value"}} → settings
//	GET    /v1/conninfo            ConnInfo (application role, all client ports, TLS)
//	POST   /v1/app-password        new application password → ConnInfo
//	GET    /v1/dev/status          this node's own report
//	POST   /v1/dev/switch          {"to":"name"} → SwitchResult
//	POST   /v1/dev/set-sync        {"standbys":["b"],"count":1}
//	POST   /v1/dev/cut-stream      cut this standby's stream until the next switch
//	POST   /v1/dev/resume-stream   undo cut-stream
//
// The dashboard's routes (dashboard/API.md) are added by d.routes; a
// session cookie may use every route except the bearer-only ones
// (sessionStore.BearerOnly).
func (a *Agent) controlHandler(d *dashServer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+control.Prefix+"/conninfo", a.apiConnInfo)
	mux.HandleFunc("POST "+control.Prefix+"/app-password", a.apiNewAppPassword)
	mux.HandleFunc("POST "+control.Prefix+"/leave", a.apiLeave)
	mux.HandleFunc("DELETE "+control.Prefix+"/members/{node}", a.apiRemove)
	mux.HandleFunc("POST "+control.Prefix+"/members/{node}/addresses", a.apiSetAddress)
	mux.HandleFunc("GET "+control.Prefix+"/dev/status", func(w http.ResponseWriter, r *http.Request) {
		control.WriteJSON(w, http.StatusOK, a.report(r.Context()))
	})
	mux.HandleFunc("POST "+control.Prefix+"/dev/switch", a.apiSwitch)
	mux.HandleFunc("POST "+control.Prefix+"/dev/set-sync", a.apiSetSync)
	mux.HandleFunc("POST "+control.Prefix+"/dev/cut-stream", func(w http.ResponseWriter, r *http.Request) {
		lsn, err := a.cutStream(r.Context(), 0, 0)
		if err != nil {
			apiErr(w, err)
			return
		}
		control.WriteJSON(w, http.StatusOK, map[string]string{"received_lsn": lsn.String(),
			"message": "Stream cut. This copy stays disconnected until the next switch (or dev resume-stream)."})
	})
	mux.HandleFunc("POST "+control.Prefix+"/dev/resume-stream", func(w http.ResponseWriter, r *http.Request) {
		if err := a.resumeStream(r.Context()); err != nil {
			apiErr(w, err)
			return
		}
		control.WriteJSON(w, http.StatusOK, map[string]string{"message": "Following the main node again."})
	})
	d.routes(mux)
	return mux
}

func apiErr(w http.ResponseWriter, err error) {
	code, status := "failed", http.StatusConflict
	var ce *cluster.Error
	var re *rpcError
	switch {
	case errors.As(err, &ce):
		code = ce.Code
	case errors.As(err, &re) && re.Code != "":
		code = re.Code
	case errors.Is(err, context.DeadlineExceeded):
		code, status = "timeout", http.StatusGatewayTimeout
	}
	control.WriteError(w, status, code, err.Error())
}

// InviteRequest / InviteResponse are POST /v1/invites.
type InviteRequest struct {
	TTL string `json:"ttl,omitempty"`
}

type InviteResponse struct {
	Code    string    `json:"code"`
	Expires time.Time `json:"expires"`
}

// createInvite is `coyote invite` (the CLI and the dashboard). On failure
// it has written the error response and returns false.
func (a *Agent) createInvite(w http.ResponseWriter, ctx context.Context, ttlText string) (InviteResponse, bool) {
	ttl := time.Hour
	if ttlText != "" {
		d, err := time.ParseDuration(ttlText)
		if err != nil || d <= 0 || d > 7*24*time.Hour {
			control.WriteError(w, http.StatusBadRequest, "bad_request", "--ttl must be a duration like 30m or 2h, at most 168h")
			return InviteResponse{}, false
		}
		ttl = d
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	code, err := a.cn.CreateInvite(ctx, ttl, nil)
	if err != nil {
		apiErr(w, fmt.Errorf("could not create an invite (the cluster must have a leader): %w", err))
		return InviteResponse{}, false
	}
	return InviteResponse{Code: code, Expires: time.Now().Add(ttl).Truncate(time.Second).UTC()}, true
}

// leaveReq is handed to the loop, which owns Postgres.
type leaveReq struct{ reply chan error }

func (a *Agent) apiLeave(w http.ResponseWriter, r *http.Request) {
	req := leaveReq{reply: make(chan error, 1)}
	select {
	case a.leaveCh <- req:
	case <-r.Context().Done():
		return
	}
	select {
	case err := <-req.reply:
		if err != nil {
			apiErr(w, err)
			return
		}
		control.WriteJSON(w, http.StatusOK, map[string]string{"message": "Left the cluster."})
	case <-r.Context().Done():
	}
}

// doLeave runs in the loop: hand over the sync set position, leave the
// cluster (Raft), then stop Postgres for good.
func (a *Agent) doLeave(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	st := a.cn.State()
	self := a.cn.ID()
	if st.PrimaryID == self {
		return fmt.Errorf("this node is the main node; move that role first (%s dev switch --to <node>), then leave", brand.Command)
	}
	if err := a.removeMember(ctx, self, a.cn.Leave); err != nil {
		return fmt.Errorf("leave the cluster: %w", err)
	}
	_ = a.stopPG()
	if err := a.st.update(func(f *agentFile) { f.Removed = "left the cluster" }); err != nil {
		return err
	}
	a.say("Left the cluster. This node's database was stopped; its data stays in %s.", a.node.StateDir)
	return nil
}

func (a *Agent) apiRemove(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	st := a.cn.State()
	m, ok := findMember(st, r.PathValue("node"))
	switch {
	case !ok:
		control.WriteError(w, http.StatusNotFound, "unknown_member", fmt.Sprintf("no node called %q in this cluster", r.PathValue("node")))
		return
	case m.ID == a.cn.ID():
		control.WriteError(w, http.StatusConflict, "is_self", fmt.Sprintf("that is this node; use `%s leave` instead", brand.Command))
		return
	case m.ID == st.PrimaryID:
		control.WriteError(w, http.StatusConflict, "is_primary", fmt.Sprintf("%s is the main node; move that role first (%s dev switch --to <node>)", m.Name, brand.Command))
		return
	}
	err := a.removeMember(ctx, m.ID, func(ctx context.Context) error { return a.cn.RemoveMember(ctx, m.ID) })
	if err != nil {
		apiErr(w, fmt.Errorf("remove %s: %w", m.Name, err))
		return
	}
	a.log.Info("member removed", "member", m.Name)
	control.WriteJSON(w, http.StatusOK, map[string]string{"message": fmt.Sprintf("Removed %s. Its database stops and it can no longer connect.", m.Name)})
}

// removeMember takes member id out of the cluster; remove is the last step
// (Node.RemoveMember, or Node.Leave for this node). D59: it first records
// that the member is being removed, and from then on the cluster state
// refuses to add it to the sync set. Otherwise the controller, which adds
// every caught-up copy, could put it back between the hand-over and the
// removal (or have that add already on its way when the remove starts),
// and the removal would be refused as "in the sync set".
//
// The cluster's answer decides, not this node's copy of the state (it can
// be a moment behind): while the removal is refused because the member is
// in the sync set, the main node hands it over (S first, a barrier, the
// other copies catch up, then R; safety rule 3) and the removal is asked
// again. With the mark in place no add can come back, so this ends. On
// failure the mark is taken back, so the member is a full copy again.
func (a *Agent) removeMember(ctx context.Context, id string, remove func(context.Context) error) error {
	yes := true
	if _, err := a.cn.Apply(ctx, &cluster.UpdateMember{ID: id, Removing: &yes}); err != nil {
		return fmt.Errorf("record the removal: %w", err)
	}
	err := a.handOverAndRemove(ctx, id, remove)
	if err != nil {
		no := false
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, uerr := a.cn.Apply(cctx, &cluster.UpdateMember{ID: id, Removing: &no}); uerr != nil && !errors.Is(uerr, cluster.ErrUnknownMember) {
			a.log.Warn("removal failed and its mark could not be taken back; running the removal again finishes it",
				"member", id, "err", uerr)
		}
	}
	return err
}

func (a *Agent) handOverAndRemove(ctx context.Context, id string, remove func(context.Context) error) error {
	for {
		err := remove(ctx)
		if !errors.Is(err, cluster.ErrInSyncSet) {
			return err
		}
		a.log.Info("removal: member is in the sync set; the main node hands it over first", "member", id)
		st := a.cn.State()
		if err := a.call(ctx, st.PrimaryID, "sync_remove", syncNodeReq{Node: id}, nil); err != nil {
			return fmt.Errorf("take it out of the sync set first: %w", err)
		}
		// If the main node's copy of the state did not yet show the member
		// in the sync set, the hand-over did nothing; ask again shortly.
		select {
		case <-ctx.Done():
			return fmt.Errorf("still in the sync set: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (a *Agent) apiSwitch(w http.ResponseWriter, r *http.Request) { a.apiSwitchMode(w, r, false) }

func (a *Agent) apiSwitchMode(w http.ResponseWriter, r *http.Request, planned bool) {
	var req switchReq
	if err := control.ReadJSON(r, &req); err != nil || req.To == "" {
		control.WriteError(w, http.StatusBadRequest, "bad_request", "usage: switch --to <node>")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	res, err := a.runSwitch(ctx, req.To, planned)
	if err != nil {
		apiErr(w, err)
		return
	}
	control.WriteJSON(w, http.StatusOK, res)
}

func (a *Agent) apiSetSync(w http.ResponseWriter, r *http.Request) {
	var req setSyncReq
	if err := control.ReadJSON(r, &req); err != nil {
		control.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	st := a.cn.State()
	if st.PrimaryID == "" {
		control.WriteError(w, http.StatusConflict, "no_primary", "no node is the main node right now")
		return
	}
	if err := a.call(ctx, st.PrimaryID, "set_sync", req, nil); err != nil {
		apiErr(w, err)
		return
	}
	control.WriteJSON(w, http.StatusOK, map[string]string{"message": "Sync set updated."})
}

// SettingsView is GET /v1/settings: the cluster settings in plain units.
type SettingsView struct {
	LeaseDuration      string `json:"lease_duration"`
	LeaseRenewInterval string `json:"lease_renew_interval"`
	SingleDataNodeMode string `json:"single_data_node_mode"`
	DegradeAfter       string `json:"degrade_after"`
	GoneAfter          string `json:"gone_after"`
}

func settingsView(s cluster.Settings) SettingsView {
	return SettingsView{
		LeaseDuration: s.LeaseDuration.String(), LeaseRenewInterval: s.LeaseRenewInterval.String(),
		SingleDataNodeMode: s.SingleDataNodeMode, DegradeAfter: s.DegradeAfter.String(), GoneAfter: s.EffectiveGoneAfter().String(),
	}
}

// SetSettingsRequest is POST /v1/settings.
type SetSettingsRequest struct {
	Set map[string]string `json:"set"`
}

// SettingKeys are the names `coyote settings set` accepts.
var SettingKeys = []string{"lease_duration", "lease_renew_interval", "single_data_node_mode", "degrade_after", "gone_after"}

// applySetting changes one setting by name.
func applySetting(s *cluster.Settings, key, val string) error {
	dur := func(dst *time.Duration) error {
		d, err := time.ParseDuration(val)
		if err != nil || d < 0 {
			return fmt.Errorf("%s must be a duration like 30s or 5m", key)
		}
		*dst = d
		return nil
	}
	switch key {
	case "lease_duration":
		return dur(&s.LeaseDuration)
	case "lease_renew_interval":
		return dur(&s.LeaseRenewInterval)
	case "degrade_after":
		return dur(&s.DegradeAfter)
	case "gone_after":
		return dur(&s.GoneAfter)
	case "single_data_node_mode":
		if val != cluster.ModeStrict && val != cluster.ModeDegradeAfter {
			return fmt.Errorf("single_data_node_mode must be %q or %q", cluster.ModeStrict, cluster.ModeDegradeAfter)
		}
		s.SingleDataNodeMode = val
		return nil
	}
	return fmt.Errorf("unknown setting %q (known: %s)", key, strings.Join(SettingKeys, ", "))
}

func (a *Agent) apiSetSettings(w http.ResponseWriter, r *http.Request) {
	var req SetSettingsRequest
	if err := control.ReadJSON(r, &req); err != nil || len(req.Set) == 0 {
		control.WriteError(w, http.StatusBadRequest, "bad_request", "usage: settings set key=value ...")
		return
	}
	s := a.cn.State().Settings
	keys := make([]string, 0, len(req.Set))
	for k := range req.Set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if err := applySetting(&s, k, req.Set[k]); err != nil {
			control.WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	if err := cluster.CheckLeaseTiming(s, a.cn.Timing()); err != nil {
		control.WriteError(w, http.StatusBadRequest, "unsafe_timing", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if _, err := a.cn.Apply(ctx, &cluster.SetSettings{Settings: s}); err != nil {
		apiErr(w, fmt.Errorf("could not change the settings (the cluster must have a leader): %w", err))
		return
	}
	a.log.Info("settings changed", "set", req.Set)
	control.WriteJSON(w, http.StatusOK, settingsView(a.cn.State().Settings))
}

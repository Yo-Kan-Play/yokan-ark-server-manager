package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type PodmanClient struct {
	http    *http.Client
	baseURL string
	cfg     *Config
}

type PodmanContainerState struct {
	Exists   bool
	Running  bool
	Status   string
	StartedAt time.Time
}

type MapStatus struct {
	Map      MapConfig
	Container string
	Exists   bool
	Running  bool
	Status   string
	MemoryBytes uint64
}

func NewPodmanClient(cfg *Config) *PodmanClient {
	dialer := &net.Dialer{Timeout: time.Duration(cfg.Runtime.PodmanTimeoutSeconds) * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", cfg.Podman.SocketPath)
		},
	}
	return &PodmanClient{
		http:    &http.Client{Transport: transport},
		baseURL: "http://d/v1.40",
		cfg:     cfg,
	}
}

func (p *PodmanClient) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return p.http.Do(req)
}

func containerName(cfg *Config, mapID string) string {
	return cfg.Podman.ContainerNamePrefix + mapID
}

func (p *PodmanClient) Ping(ctx context.Context) error {
	resp, err := p.request(ctx, http.MethodGet, "/_ping", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("podman ping失敗: %s: %s", resp.Status, string(buf))
	}
	return nil
}

func (p *PodmanClient) InspectState(ctx context.Context, mapID string) (PodmanContainerState, error) {
	name := containerName(p.cfg, mapID)
	resp, err := p.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil)
	if err != nil {
		return PodmanContainerState{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return PodmanContainerState{Exists: false}, nil
	}
	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(resp.Body)
		return PodmanContainerState{}, fmt.Errorf("inspect失敗: %s: %s", resp.Status, string(buf))
	}
	var payload struct {
		State struct {
			Running   bool   `json:"Running"`
			Status    string `json:"Status"`
			StartedAt string `json:"StartedAt"`
		} `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return PodmanContainerState{}, err
	}
	startedAt := time.Time{}
	if payload.State.StartedAt != "" {
		t, _ := time.Parse(time.RFC3339Nano, payload.State.StartedAt)
		startedAt = t
	}
	return PodmanContainerState{Exists: true, Running: payload.State.Running, Status: payload.State.Status, StartedAt: startedAt}, nil
}

func (p *PodmanClient) CreateContainer(ctx context.Context, m MapConfig) error {
	name := containerName(p.cfg, m.MapID)
	rconPort := m.Port + p.cfg.Server.RCONPortOffset
	queryPort := m.Port + p.cfg.Server.QueryPortOffset
	memoryLimitGB := p.cfg.Server.MemoryLimitGB
	if m.MemoryLimitGB != nil {
		memoryLimitGB = *m.MemoryLimitGB
	}
	publishQuery := p.cfg.Podman.CreateDefaults.PublishQueryPort
	if m.PublishQueryPort != nil {
		publishQuery = *m.PublishQueryPort
	}

	env := []string{
		"MAP_ID=" + m.MapID,
		"SESSION_NAME=" + m.SessionName,
		"PORT=" + strconv.Itoa(m.Port),
		"CLUSTER_ID=" + p.cfg.Server.ClusterID,
		"MAX_PLAYERS=" + strconv.Itoa(p.cfg.Server.MaxPlayers),
	}

	hostCfg := map[string]any{
		"Binds": []string{p.cfg.Podman.PersistHostPath + ":/persist:rw"},
		"Memory": int64(memoryLimitGB) * 1024 * 1024 * 1024,
		"PortBindings": map[string][]map[string]string{
			fmt.Sprintf("%d/udp", m.Port): {{"HostPort": strconv.Itoa(m.Port)}},
			fmt.Sprintf("%d/tcp", rconPort): {{"HostPort": strconv.Itoa(rconPort)}},
		},
	}
	if publishQuery {
		pb := hostCfg["PortBindings"].(map[string][]map[string]string)
		pb[fmt.Sprintf("%d/udp", queryPort)] = []map[string]string{{"HostPort": strconv.Itoa(queryPort)}}
	}
	if p.cfg.Podman.CreateDefaults.TimezoneMount {
		hostCfg["Binds"] = append(hostCfg["Binds"].([]string), "/etc/localtime:/etc/localtime:ro")
	}

	exposed := map[string]map[string]struct{}{
		fmt.Sprintf("%d/udp", m.Port):   {},
		fmt.Sprintf("%d/tcp", rconPort): {},
	}
	if publishQuery {
		exposed[fmt.Sprintf("%d/udp", queryPort)] = map[string]struct{}{}
	}

	body := map[string]any{
		"Image":        p.cfg.Podman.ServerImage,
		"Env":          env,
		"ExposedPorts": exposed,
		"HostConfig":   hostCfg,
	}
	resp, err := p.request(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return nil
	}
	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create失敗: %s: %s", resp.Status, string(buf))
	}
	return nil
}

func (p *PodmanClient) Start(ctx context.Context, mapID string) error {
	name := containerName(p.cfg, mapID)
	resp, err := p.request(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("start失敗: %s: %s", resp.Status, string(buf))
	}
	return nil
}

func (p *PodmanClient) Stop(ctx context.Context, mapID string) error {
	name := containerName(p.cfg, mapID)
	resp, err := p.request(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/stop", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("stop失敗: %s: %s", resp.Status, string(buf))
	}
	return nil
}

func (p *PodmanClient) StatsMemory(ctx context.Context, mapID string) (uint64, error) {
	name := containerName(p.cfg, mapID)
	resp, err := p.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/stats?stream=false", nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, nil
	}
	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("stats失敗: %s: %s", resp.Status, string(buf))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, err
	}
	memStats, ok := payload["memory_stats"].(map[string]any)
	if !ok {
		return 0, nil
	}
	usage, ok := memStats["usage"].(float64)
	if !ok {
		return 0, nil
	}
	return uint64(usage), nil
}

func (p *PodmanClient) RunningCount(ctx context.Context) (int, error) {
	resp, err := p.request(ctx, http.MethodGet, "/containers/json?all=1", nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("containers一覧失敗: %s: %s", resp.Status, string(buf))
	}
	var arr []struct {
		Names []string `json:"Names"`
		State string `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		return 0, err
	}
	count := 0
	for _, c := range arr {
		if c.State != "running" {
			continue
		}
		for _, n := range c.Names {
			if strings.TrimPrefix(n, "/") == "" {
				continue
			}
			if strings.HasPrefix(strings.TrimPrefix(n, "/"), p.cfg.Podman.ContainerNamePrefix) {
				count++
				break
			}
		}
	}
	return count, nil
}

func (p *PodmanClient) CollectStatuses(ctx context.Context, maps []MapConfig) ([]MapStatus, error) {
	out := make([]MapStatus, 0, len(maps))
	var errs []error
	for _, m := range maps {
		st, err := p.InspectState(ctx, m.MapID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s inspect: %w", m.MapID, err))
			out = append(out, MapStatus{Map: m, Container: containerName(p.cfg, m.MapID)})
			continue
		}
		mem := uint64(0)
		if st.Running {
			if v, err := p.StatsMemory(ctx, m.MapID); err == nil {
				mem = v
			}
		}
		out = append(out, MapStatus{
			Map: m, Container: containerName(p.cfg, m.MapID), Exists: st.Exists, Running: st.Running, Status: st.Status, MemoryBytes: mem,
		})
	}
	return out, errors.Join(errs...)
}

func (p *PodmanClient) DefaultSaveDir(mapID string) string {
	return filepath.Join(p.cfg.Podman.PersistContainerPath, "maps", mapID, "server-files", "ShooterGame", "Saved")
}

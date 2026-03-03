package main

import (
	"fmt"
	"os"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Discord      DiscordConfig      `yaml:"discord"`
	Permissions  PermissionsConfig  `yaml:"permissions"`
	Runtime      RuntimeConfig      `yaml:"runtime"`
	Podman       PodmanConfig       `yaml:"podman"`
	Server       ServerDefaults     `yaml:"server_defaults"`
	Backup       BackupConfig       `yaml:"backup"`
	Announcements AnnouncementConfig `yaml:"announcements"`
	PreShutdown  PreShutdownConfig  `yaml:"pre_shutdown"`
	ShutdownGuard ShutdownGuardConfig `yaml:"shutdown_guard"`
	Maps         []MapConfig        `yaml:"maps"`
}

type DiscordConfig struct {
	CommandChannelIDs []string `yaml:"command_channel_ids"`
	NotifyChannelID   string   `yaml:"notify_channel_id"`
	CommandGuildID    string   `yaml:"command_guild_id"`
}

type PermissionsConfig struct {
	Default  PermissionRule            `yaml:"default"`
	Commands map[string]PermissionRule `yaml:"commands"`
}

type PermissionRule struct {
	AllowRoleIDs []string `yaml:"allow_role_ids"`
	AllowUserIDs []string `yaml:"allow_user_ids"`
}

type RuntimeConfig struct {
	MaxRunningMaps      int `yaml:"max_running_maps"`
	ScanIntervalMinutes int `yaml:"scan_interval_minutes"`
	IdleStopMinutes     int `yaml:"idle_stop_minutes"`
	SaveWaitSeconds     int `yaml:"save_wait_seconds"`
	RCONTimeoutSeconds  int `yaml:"rcon_timeout_seconds"`
	PodmanTimeoutSeconds int `yaml:"podman_timeout_seconds"`
	PresenceIntervalSeconds int `yaml:"presence_interval_seconds"`
}

type PodmanConfig struct {
	SocketPath          string `yaml:"socket_path"`
	ServerImage         string `yaml:"server_image"`
	PersistHostPath     string `yaml:"persist_host_path"`
	PersistContainerPath string `yaml:"persist_container_path"`
	ContainerNamePrefix string `yaml:"container_name_prefix"`
	CreateDefaults      struct {
		TimezoneMount     bool `yaml:"timezone_mount"`
		PublishQueryPort  bool `yaml:"publish_query_port"`
	} `yaml:"create_defaults"`
}

type ServerDefaults struct {
	MaxPlayers      int    `yaml:"max_players"`
	ClusterID       string `yaml:"cluster_id"`
	MemoryLimitGB   int    `yaml:"memory_limit_gb"`
	RCONPasswordEnv string `yaml:"rcon_password_env"`
	RCONHost        string `yaml:"rcon_host"`
	RCONPortOffset  int    `yaml:"rcon_port_offset"`
	QueryPortOffset int    `yaml:"query_port_offset"`
}

type BackupConfig struct {
	Local struct {
		Enabled         bool   `yaml:"enabled"`
		IntervalMinutes int    `yaml:"interval_minutes"`
		KeepGenerations int    `yaml:"keep_generations"`
		RunOnStop       bool   `yaml:"run_on_stop"`
		OutDir          string `yaml:"out_dir"`
	} `yaml:"local"`
	Cloud struct {
		Enabled       bool   `yaml:"enabled"`
		ScheduleHHMM  string `yaml:"schedule_hhmm"`
		KeepDays      int    `yaml:"keep_days"`
		Provider      string `yaml:"provider"`
		Bucket        string `yaml:"bucket"`
		Prefix        string `yaml:"prefix"`
	} `yaml:"cloud"`
}

type AnnouncementConfig struct {
	Enabled bool `yaml:"enabled"`
	Items   []AnnouncementItem `yaml:"items"`
}

type AnnouncementItem struct {
	Name        string   `yaml:"name"`
	Time        string   `yaml:"time"`
	Weekdays    []string `yaml:"weekdays"`
	Message     string   `yaml:"message"`
	IncludeMaps []string `yaml:"include_maps"`
}

type PreShutdownConfig struct {
	Enabled bool `yaml:"enabled"`
	Time    string `yaml:"time"`
	Action  string `yaml:"action"`
}

type ShutdownGuardConfig struct {
	Enabled    bool `yaml:"enabled"`
	BestEffort bool `yaml:"best_effort"`
}

type MapConfig struct {
	MapID            string `yaml:"map_id"`
	DisplayName      string `yaml:"display_name"`
	SessionName      string `yaml:"session_name"`
	Port             int    `yaml:"port"`
	MemoryLimitGB    *int   `yaml:"memory_limit_gb"`
	BackupEnabled    *bool  `yaml:"backup_enabled"`
	SaveDirOverride  string `yaml:"save_dir_override"`
	PublishQueryPort *bool  `yaml:"publish_query_port"`
}

func LoadConfig(path string) (*Config, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config読み込み失敗: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(buf, cfg); err != nil {
		return nil, fmt.Errorf("config解析失敗: %w", err)
	}
	applyDefaults(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Runtime.MaxRunningMaps <= 0 {
		cfg.Runtime.MaxRunningMaps = 2
	}
	if cfg.Runtime.ScanIntervalMinutes <= 0 {
		cfg.Runtime.ScanIntervalMinutes = 15
	}
	if cfg.Runtime.IdleStopMinutes <= 0 {
		cfg.Runtime.IdleStopMinutes = 30
	}
	if cfg.Runtime.SaveWaitSeconds <= 0 {
		cfg.Runtime.SaveWaitSeconds = 10
	}
	if cfg.Runtime.RCONTimeoutSeconds <= 0 {
		cfg.Runtime.RCONTimeoutSeconds = 5
	}
	if cfg.Runtime.PodmanTimeoutSeconds <= 0 {
		cfg.Runtime.PodmanTimeoutSeconds = 20
	}
	if cfg.Runtime.PresenceIntervalSeconds <= 0 {
		cfg.Runtime.PresenceIntervalSeconds = 120
	}
	if cfg.Podman.ContainerNamePrefix == "" {
		cfg.Podman.ContainerNamePrefix = "yokan-ark-"
	}
	if cfg.Podman.PersistContainerPath == "" {
		cfg.Podman.PersistContainerPath = cfg.Podman.PersistHostPath
	}
	if cfg.Server.MaxPlayers <= 0 {
		cfg.Server.MaxPlayers = 10
	}
	if cfg.Server.ClusterID == "" {
		cfg.Server.ClusterID = "yokan-ark"
	}
	if cfg.Server.MemoryLimitGB <= 0 {
		cfg.Server.MemoryLimitGB = 20
	}
	if cfg.Server.RCONPasswordEnv == "" {
		cfg.Server.RCONPasswordEnv = "ARK_RCON_PASSWORD"
	}
	if cfg.Server.RCONHost == "" {
		cfg.Server.RCONHost = "host.containers.internal"
	}
	if cfg.Server.RCONPortOffset == 0 {
		cfg.Server.RCONPortOffset = 19243
	}
	if cfg.Server.QueryPortOffset == 0 {
		cfg.Server.QueryPortOffset = 1
	}
	if cfg.Backup.Local.Enabled && cfg.Backup.Local.OutDir == "" {
		cfg.Backup.Local.OutDir = "/srv/yokan-ark/backups/local"
	}
	if cfg.Backup.Local.Enabled && cfg.Backup.Local.IntervalMinutes <= 0 {
		cfg.Backup.Local.IntervalMinutes = 120
	}
	if cfg.Backup.Local.Enabled && cfg.Backup.Local.KeepGenerations <= 0 {
		cfg.Backup.Local.KeepGenerations = 3
	}
	if cfg.Backup.Cloud.Enabled && cfg.Backup.Cloud.Provider == "" {
		cfg.Backup.Cloud.Provider = "cloudflare_r2"
	}
}

func (c *Config) Validate() error {
	if len(c.Discord.CommandChannelIDs) == 0 {
		return fmt.Errorf("discord.command_channel_ids は1件以上必須です")
	}
	if c.Discord.NotifyChannelID == "" {
		return fmt.Errorf("discord.notify_channel_id は必須です")
	}
	if c.Podman.SocketPath == "" || c.Podman.ServerImage == "" || c.Podman.PersistHostPath == "" {
		return fmt.Errorf("podman.socket_path / podman.server_image / podman.persist_host_path は必須です")
	}
	if len(c.Maps) == 0 {
		return fmt.Errorf("maps は1件以上必須です")
	}
	ports := map[int]string{}
	for _, m := range c.Maps {
		if m.MapID == "" || m.DisplayName == "" || m.SessionName == "" || m.Port <= 0 {
			return fmt.Errorf("maps の map_id/display_name/session_name/port は必須です")
		}
		if prev, ok := ports[m.Port]; ok {
			return fmt.Errorf("port重複: %d (%s, %s)", m.Port, prev, m.MapID)
		}
		ports[m.Port] = m.MapID
	}
	if c.Backup.Cloud.Enabled {
		if c.Backup.Cloud.ScheduleHHMM == "" || c.Backup.Cloud.Bucket == "" {
			return fmt.Errorf("backup.cloud.enabled=true の場合 schedule_hhmm と bucket は必須です")
		}
		if _, err := time.Parse("15:04", c.Backup.Cloud.ScheduleHHMM); err != nil {
			return fmt.Errorf("backup.cloud.schedule_hhmm は HH:MM 形式: %w", err)
		}
	}
	if c.PreShutdown.Enabled && c.PreShutdown.Time != "" {
		if _, err := time.Parse("15:04", c.PreShutdown.Time); err != nil {
			return fmt.Errorf("pre_shutdown.time は HH:MM 形式: %w", err)
		}
	}
	return nil
}

func (c *Config) MapByID() map[string]MapConfig {
	out := make(map[string]MapConfig, len(c.Maps))
	for _, m := range c.Maps {
		out[m.MapID] = m
	}
	return out
}

func (c *Config) SortedMapIDs() []string {
	ids := make([]string, 0, len(c.Maps))
	for _, m := range c.Maps {
		ids = append(ids, m.MapID)
	}
	sort.Strings(ids)
	return ids
}

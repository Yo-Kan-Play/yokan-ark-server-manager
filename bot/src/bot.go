package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

type Bot struct {
	cfg      *Config
	log      *ActionLogger
	dg       *discordgo.Session
	podman   *PodmanClient
	rcon     *RCONClient
	backup   *BackupManager
	mapByID  map[string]MapConfig

	mu              sync.Mutex
	mapLocks        map[string]*sync.Mutex
	zeroSince       map[string]time.Time
	startedAt       map[string]time.Time
	nextLocalBackup map[string]time.Time
	lastScheduleRun map[string]time.Time
	scanQueue       chan string
}

func NewBot(cfg *Config, logger *ActionLogger) (*Bot, error) {
	token := strings.TrimSpace(os.Getenv("DISCORD_TOKEN"))
	if token == "" {
		return nil, fmt.Errorf("DISCORD_TOKEN が未設定です")
	}
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	dg.Identify.Intents = discordgo.IntentsGuilds
	b := &Bot{
		cfg:            cfg,
		log:            logger,
		dg:             dg,
		podman:         NewPodmanClient(cfg),
		rcon:           NewRCONClient(cfg),
		backup:         NewBackupManager(cfg, NewPodmanClient(cfg)),
		mapByID:        cfg.MapByID(),
		mapLocks:       map[string]*sync.Mutex{},
		zeroSince:      map[string]time.Time{},
		startedAt:      map[string]time.Time{},
		nextLocalBackup: map[string]time.Time{},
		lastScheduleRun: map[string]time.Time{},
		scanQueue:      make(chan string, 1),
	}
	for _, m := range cfg.Maps {
		b.mapLocks[m.MapID] = &sync.Mutex{}
	}
	return b, nil
}

func (b *Bot) mapLock(mapID string) *sync.Mutex {
	if lock, ok := b.mapLocks[mapID]; ok {
		return lock
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if lock, ok := b.mapLocks[mapID]; ok {
		return lock
	}
	lock := &sync.Mutex{}
	b.mapLocks[mapID] = lock
	return lock
}

func (b *Bot) Run(ctx context.Context) error {
	ctxPing, cancelPing := context.WithTimeout(ctx, time.Duration(b.cfg.Runtime.PodmanTimeoutSeconds)*time.Second)
	defer cancelPing()
	if err := b.podman.Ping(ctxPing); err != nil {
		return fmt.Errorf("podman socket接続失敗: %w", err)
	}
	b.dg.AddHandler(b.onReady)
	b.dg.AddHandler(b.onInteractionCreate)
	if err := b.dg.Open(); err != nil {
		return err
	}
	defer b.dg.Close()

	if err := b.registerCommands(); err != nil {
		return err
	}
	b.restoreRuntimeState(ctx)
	b.log.Info("Discord Bot 起動完了")
	go b.runScanWorker(ctx)
	go b.runSchedulers(ctx)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case sig := <-sigCh:
		b.log.Warn("シグナル受信: %s", sig.String())
		if b.cfg.ShutdownGuard.Enabled && b.cfg.ShutdownGuard.BestEffort {
			_ = b.gracefulStopAll(context.Background(), "shutdown_guard")
		}
	}
	return nil
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	b.log.Info("Discord接続: %s", r.User.Username)
	_ = s.UpdateCustomStatus("ARK Bot 準備完了")
}

func (b *Bot) registerCommands() error {
	cmd := &discordgo.ApplicationCommand{
		Name:        "ark",
		Description: "ARKサーバー管理",
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "start", Description: "マップ起動", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "map", Description: "map_id", Required: true}}},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "stop", Description: "マップ停止", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "map", Description: "map_id", Required: true}}},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "status", Description: "状態表示"},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "save", Description: "全起動マップ保存"},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "backup", Description: "全起動マップバックアップ"},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "scan", Description: "無人監視を即時実行"},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "players", Description: "プレイヤー一覧", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "map", Description: "map_id", Required: false}}},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "maps", Description: "マップ一覧"},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "broadcast", Description: "全起動マップへ送信", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "msg", Description: "メッセージ", Required: true}}},
		},
	}
	guildID := strings.TrimSpace(b.cfg.Discord.CommandGuildID)
	_, err := b.dg.ApplicationCommandCreate(b.dg.State.User.ID, guildID, cmd)
	return err
}

func (b *Bot) allowedChannel(channelID string) bool {
	for _, v := range b.cfg.Discord.CommandChannelIDs {
		if v == channelID {
			return true
		}
	}
	return false
}

func (b *Bot) permissionAllowed(member *discordgo.Member, userID, command string) bool {
	rule, ok := b.cfg.Permissions.Commands[command]
	if ok && len(rule.AllowUserIDs) > 0 {
		for _, uid := range rule.AllowUserIDs {
			if uid == userID {
				return true
			}
		}
		return false
	}
	if len(b.cfg.Permissions.Default.AllowRoleIDs) > 0 && member != nil {
		for _, rid := range member.Roles {
			for _, allowed := range b.cfg.Permissions.Default.AllowRoleIDs {
				if rid == allowed {
					return true
				}
			}
		}
	}
	safe := map[string]bool{"status": true, "maps": true, "players": true}
	return safe[command]
}

func (b *Bot) onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	if i.ApplicationCommandData().Name != "ark" {
		return
	}
	if !b.allowedChannel(i.ChannelID) {
		b.reply(i, "このチャンネルではコマンドを受け付けません")
		return
	}
	opts := i.ApplicationCommandData().Options
	if len(opts) == 0 {
		b.reply(i, "コマンド形式が不正です")
		return
	}
	sub := opts[0]
	cmd := sub.Name
	if !b.permissionAllowed(i.Member, i.Member.User.ID, cmd) {
		b.reply(i, "このコマンドを実行する権限がありません")
		return
	}
	if !b.deferReply(i) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var message string
	var err error
	switch cmd {
	case "start":
		message, err = b.cmdStart(ctx, sub.Options[0].StringValue())
	case "stop":
		message, err = b.cmdStop(ctx, sub.Options[0].StringValue(), true)
	case "status":
		message, err = b.cmdStatus(ctx)
	case "save":
		message, err = b.cmdSave(ctx)
	case "backup":
		message, err = b.cmdBackup(ctx)
	case "scan":
		message, err = b.cmdScan(ctx)
	case "players":
		mapID := ""
		if len(sub.Options) > 0 {
			mapID = sub.Options[0].StringValue()
		}
		message, err = b.cmdPlayers(ctx, mapID)
	case "maps":
		message, err = b.cmdMaps(ctx)
	case "broadcast":
		message, err = b.cmdBroadcast(ctx, sub.Options[0].StringValue())
	default:
		message = "未対応コマンドです"
	}
	if err != nil {
		b.editReply(i, message+"\nエラー: "+err.Error())
		return
	}
	b.editReply(i, message)
}

func (b *Bot) deferReply(i *discordgo.InteractionCreate) bool {
	err := b.dg.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	if err == nil {
		return true
	}
	b.log.Warn("defer応答失敗: %v", err)
	return false
}

func (b *Bot) editReply(i *discordgo.InteractionCreate, msg string) {
	_, err := b.dg.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
	if err != nil {
		b.log.Warn("応答編集失敗: %v", err)
	}
}

func (b *Bot) listPlayersLocked(ctx context.Context, m MapConfig) ([]string, error) {
	lock := b.mapLock(m.MapID)
	lock.Lock()
	defer lock.Unlock()
	return b.rcon.ListPlayers(ctx, m)
}

func (b *Bot) broadcastLocked(ctx context.Context, m MapConfig, msg string) error {
	lock := b.mapLock(m.MapID)
	lock.Lock()
	defer lock.Unlock()
	return b.rcon.Broadcast(ctx, m, msg)
}

func (b *Bot) enqueueScan(reason string) bool {
	select {
	case b.scanQueue <- reason:
		return true
	default:
		return false
	}
}

func (b *Bot) reply(i *discordgo.InteractionCreate, msg string) {
	_ = b.dg.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: msg},
	})
}

func (b *Bot) notify(msg string) {
	if b.cfg.Discord.NotifyChannelID == "" {
		return
	}
	_, err := b.dg.ChannelMessageSend(b.cfg.Discord.NotifyChannelID, msg)
	if err != nil {
		b.log.Warn("通知失敗: %v", err)
	}
}

func (b *Bot) getMap(mapID string) (MapConfig, error) {
	m, ok := b.mapByID[mapID]
	if !ok {
		return MapConfig{}, fmt.Errorf("map未定義: %s", mapID)
	}
	return m, nil
}

func (b *Bot) cmdStart(ctx context.Context, mapID string) (string, error) {
	started := time.Now()
	m, err := b.getMap(mapID)
	if err != nil {
		return "", err
	}
	lock := b.mapLock(mapID)
	lock.Lock()
	defer lock.Unlock()

	runningCount, err := b.podman.RunningCount(ctx)
	if err != nil {
		return "", err
	}
	state, err := b.podman.InspectState(ctx, mapID)
	if err != nil {
		return "", err
	}
	if state.Running {
		return fmt.Sprintf("すでに起動しています: %s", m.DisplayName), nil
	}
	if !state.Exists && runningCount >= b.cfg.Runtime.MaxRunningMaps {
		return fmt.Sprintf("同時起動上限(%d)のため起動できません", b.cfg.Runtime.MaxRunningMaps), nil
	}
	if !state.Exists {
		if err := b.podman.CreateContainer(ctx, m); err != nil {
			b.log.Action(mapID, "create", "failed", started)
			return fmt.Sprintf("マップの起動に失敗しました: %s", m.DisplayName), err
		}
	}
	if !state.Exists && runningCount >= b.cfg.Runtime.MaxRunningMaps {
		return fmt.Sprintf("同時起動上限(%d)のため起動できません", b.cfg.Runtime.MaxRunningMaps), nil
	}
	if !state.Running {
		if runningCount >= b.cfg.Runtime.MaxRunningMaps {
			return fmt.Sprintf("同時起動上限(%d)のため起動できません", b.cfg.Runtime.MaxRunningMaps), nil
		}
		if err := b.podman.Start(ctx, mapID); err != nil {
			b.log.Action(mapID, "start", "failed", started)
			b.notify("起動失敗: " + m.DisplayName)
			return fmt.Sprintf("マップの起動に失敗しました: %s", m.DisplayName), err
		}
	}
	b.mu.Lock()
	b.startedAt[mapID] = time.Now()
	if b.cfg.Backup.Local.Enabled {
		b.nextLocalBackup[mapID] = time.Now().Add(time.Duration(b.cfg.Backup.Local.IntervalMinutes) * time.Minute)
	}
	b.mu.Unlock()
	b.log.Action(mapID, "start", "ok", started)
	b.notify("起動成功: " + m.DisplayName)
	return fmt.Sprintf("マップを起動しました: %s", m.DisplayName), nil
}

func (b *Bot) cmdStop(ctx context.Context, mapID string, runBackup bool) (string, error) {
	started := time.Now()
	m, err := b.getMap(mapID)
	if err != nil {
		return "", err
	}
	lock := b.mapLock(mapID)
	lock.Lock()
	defer lock.Unlock()
	state, err := b.podman.InspectState(ctx, mapID)
	if err != nil {
		return "", err
	}
	if !state.Exists || !state.Running {
		return fmt.Sprintf("停止済みです: %s", m.DisplayName), nil
	}
	_ = b.rcon.SaveWorld(ctx, m)
	time.Sleep(time.Duration(b.cfg.Runtime.SaveWaitSeconds) * time.Second)
	if err := b.podman.Stop(ctx, mapID); err != nil {
		b.log.Action(mapID, "stop", "failed", started)
		b.notify("停止失敗: " + m.DisplayName)
		return fmt.Sprintf("マップ停止に失敗しました: %s", m.DisplayName), err
	}
	if runBackup && b.cfg.Backup.Local.Enabled && b.cfg.Backup.Local.RunOnStop {
		_, _, _ = b.backup.LocalBackup(ctx, m)
	}
	b.mu.Lock()
	delete(b.zeroSince, mapID)
	delete(b.startedAt, mapID)
	delete(b.nextLocalBackup, mapID)
	b.mu.Unlock()
	b.log.Action(mapID, "stop", "ok", started)
	b.notify("停止成功: " + m.DisplayName)
	return fmt.Sprintf("マップを停止しました: %s", m.DisplayName), nil
}

func (b *Bot) cmdSave(ctx context.Context) (string, error) {
	status, err := b.podman.CollectStatuses(ctx, b.cfg.Maps)
	if err != nil {
		b.log.Warn("status収集中エラー: %v", err)
	}
	oks := []string{}
	fails := []string{}
	for _, st := range status {
		if !st.Running {
			continue
		}
		lock := b.mapLock(st.Map.MapID)
		lock.Lock()
		err := b.rcon.SaveWorld(ctx, st.Map)
		lock.Unlock()
		if err != nil {
			fails = append(fails, st.Map.MapID)
			continue
		}
		oks = append(oks, st.Map.MapID)
	}
	if len(oks) == 0 && len(fails) == 0 {
		return "起動中マップがありません", nil
	}
	if len(fails) > 0 {
		return fmt.Sprintf("保存を実行しました。成功: %s / 失敗: %s", strings.Join(oks, ","), strings.Join(fails, ",")), nil
	}
	return fmt.Sprintf("保存を実行しました。対象: %s", strings.Join(oks, ",")), nil
}

func (b *Bot) cmdBackup(ctx context.Context) (string, error) {
	status, _ := b.podman.CollectStatuses(ctx, b.cfg.Maps)
	failed := []string{}
	success := []string{}
	skipped := []string{}
	for _, st := range status {
		if !st.Running {
			continue
		}
		lock := b.mapLock(st.Map.MapID)
		lock.Lock()
		errSave := b.rcon.SaveWorld(ctx, st.Map)
		time.Sleep(time.Duration(b.cfg.Runtime.SaveWaitSeconds) * time.Second)
		_, _, errBackup := b.backup.LocalBackup(ctx, st.Map)
		lock.Unlock()
		if errSave != nil {
			failed = append(failed, st.Map.MapID)
			continue
		}
		if errors.Is(errBackup, ErrBackupSkipped) {
			skipped = append(skipped, st.Map.MapID)
			continue
		}
		if errBackup != nil {
			failed = append(failed, st.Map.MapID)
			continue
		}
		success = append(success, st.Map.MapID)
	}
	if len(success) == 0 && len(failed) == 0 && len(skipped) == 0 {
		return "起動中マップがありません", nil
	}
	if len(failed) > 0 {
		b.notify("ローカルバックアップ失敗: " + strings.Join(failed, ","))
	}
	if len(success) > 0 {
		b.notify("ローカルバックアップ成功: " + strings.Join(success, ","))
	}
	if len(skipped) > 0 {
		b.notify("ローカルバックアップスキップ: " + strings.Join(skipped, ","))
	}
	return fmt.Sprintf("バックアップ結果 成功:%s 失敗:%s スキップ:%s",
		strings.Join(success, ","),
		strings.Join(failed, ","),
		strings.Join(skipped, ",")), nil
}

func (b *Bot) cmdScan(ctx context.Context) (string, error) {
	_ = ctx
	if b.enqueueScan("manual") {
		return "無人監視スキャンをキュー投入しました", nil
	}
	return "無人監視スキャンはすでに実行待ちです（重複投入をスキップしました）", nil
}

func (b *Bot) cmdPlayers(ctx context.Context, mapID string) (string, error) {
	maps := []MapConfig{}
	if strings.TrimSpace(mapID) != "" {
		m, err := b.getMap(mapID)
		if err != nil {
			return "", err
		}
		maps = append(maps, m)
	} else {
		status, _ := b.podman.CollectStatuses(ctx, b.cfg.Maps)
		for _, st := range status {
			if st.Running {
				maps = append(maps, st.Map)
			}
		}
	}
	if len(maps) == 0 {
		return "起動中マップがありません", nil
	}
	lines := []string{}
	for _, m := range maps {
		players, err := b.listPlayersLocked(ctx, m)
		if err != nil {
			lines = append(lines, fmt.Sprintf("- %s: 取得失敗", m.DisplayName))
			continue
		}
		if len(players) == 0 {
			lines = append(lines, fmt.Sprintf("- %s: 0人", m.DisplayName))
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s: %d人", m.DisplayName, len(players)))
		for _, p := range players {
			lines = append(lines, "  - "+p)
		}
	}
	return "プレイヤー一覧\n" + strings.Join(lines, "\n"), nil
}

func (b *Bot) cmdMaps(ctx context.Context) (string, error) {
	status, _ := b.podman.CollectStatuses(ctx, b.cfg.Maps)
	lines := []string{}
	for _, st := range status {
		state := "停止"
		if st.Running {
			state = "起動中"
		}
		lines = append(lines, fmt.Sprintf("- %s (%s): %s", st.Map.DisplayName, st.Map.MapID, state))
	}
	return "マップ一覧\n" + strings.Join(lines, "\n"), nil
}

func (b *Bot) cmdBroadcast(ctx context.Context, msg string) (string, error) {
	status, _ := b.podman.CollectStatuses(ctx, b.cfg.Maps)
	targets := []MapConfig{}
	for _, st := range status {
		if st.Running {
			targets = append(targets, st.Map)
		}
	}
	if len(targets) == 0 {
		return "起動中マップがないため送信しませんでした", nil
	}
	failed := []string{}
	for _, m := range targets {
		err := b.broadcastLocked(ctx, m, msg)
		if err != nil {
			failed = append(failed, m.MapID)
		}
	}
	if len(failed) > 0 {
		return fmt.Sprintf("メッセージ送信に失敗しました（失敗: %s）", strings.Join(failed, ",")), nil
	}
	return fmt.Sprintf("起動中の全マップにメッセージを送信しました（対象: %dマップ）", len(targets)), nil
}

func (b *Bot) cmdStatus(ctx context.Context) (string, error) {
	status, err := b.podman.CollectStatuses(ctx, b.cfg.Maps)
	if err != nil {
		b.log.Warn("status collect warning: %v", err)
	}
	running := 0
	lines := []string{}
	for _, st := range status {
		if !st.Running {
			continue
		}
		running++
		players, _ := b.listPlayersLocked(ctx, st.Map)
		saveSize, _ := dirSize(b.backup.saveDir(st.Map))
		_, localSize, _ := b.backup.latestLocalBackup(st.Map)
		cloudSize, _ := b.backup.LatestCloudSize(ctx, st.Map)
		nextBackup := "-"
		b.mu.Lock()
		nb := b.nextLocalBackup[st.Map.MapID]
		b.mu.Unlock()
		if !nb.IsZero() {
			nextBackup = nb.In(mustJSTNow().Location()).Format("15:04")
		}
		lines = append(lines,
			fmt.Sprintf("- %s: %d人 / メモリ %s / セーブ %s / 最新ローカル %s / 最新クラウド %s / 次回ローカル %s",
				st.Map.DisplayName,
				len(players),
				bytesToHuman(int64(st.MemoryBytes)),
				bytesToHuman(saveSize),
				bytesToHuman(localSize),
				bytesToHuman(cloudSize),
				nextBackup,
			),
		)
	}
	if running == 0 {
		return fmt.Sprintf("起動中マップ: 0/%d", b.cfg.Runtime.MaxRunningMaps), nil
	}
	head := fmt.Sprintf("起動中マップ: %d/%d", running, b.cfg.Runtime.MaxRunningMaps)
	return head + "\n" + strings.Join(lines, "\n"), nil
}

func (b *Bot) runScanWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case reason := <-b.scanQueue:
			_ = b.scanIdleOnce(context.Background(), reason)
		}
	}
}

func (b *Bot) scanIdleOnce(ctx context.Context, reason string) error {
	status, err := b.podman.CollectStatuses(ctx, b.cfg.Maps)
	if err != nil {
		b.log.Warn("scan status warning: %v", err)
	}
	now := time.Now()
	stopped := []string{}
	for _, st := range status {
		if !st.Running {
			b.mu.Lock()
			delete(b.zeroSince, st.Map.MapID)
			b.mu.Unlock()
			continue
		}
		lock := b.mapLock(st.Map.MapID)
		lock.Lock()
		players, perr := b.rcon.ListPlayers(ctx, st.Map)
		lock.Unlock()
		if perr != nil {
			continue
		}
		b.mu.Lock()
		zeroAt := b.zeroSince[st.Map.MapID]
		if len(players) == 0 {
			if zeroAt.IsZero() {
				b.zeroSince[st.Map.MapID] = now
				zeroAt = now
			}
		} else {
			delete(b.zeroSince, st.Map.MapID)
		}
		b.mu.Unlock()
		if len(players) > 0 {
			continue
		}
		if now.Sub(zeroAt) >= time.Duration(b.cfg.Runtime.IdleStopMinutes)*time.Minute {
			if _, err := b.cmdStop(ctx, st.Map.MapID, true); err == nil {
				stopped = append(stopped, st.Map.DisplayName)
			}
		}
	}
	if len(stopped) > 0 {
		b.notify(fmt.Sprintf("無人停止を実行しました (%s) reason=%s", strings.Join(stopped, ","), reason))
	}
	return nil
}

func (b *Bot) runSchedulers(ctx context.Context) {
	scanTicker := time.NewTicker(time.Duration(b.cfg.Runtime.ScanIntervalMinutes) * time.Minute)
	presenceTicker := time.NewTicker(time.Duration(b.cfg.Runtime.PresenceIntervalSeconds) * time.Second)
	minuteTicker := time.NewTicker(1 * time.Minute)
	defer scanTicker.Stop()
	defer presenceTicker.Stop()
	defer minuteTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-scanTicker.C:
			_ = b.enqueueScan("interval")
		case <-presenceTicker.C:
			_ = b.updatePresence(ctx)
		case <-minuteTicker.C:
			b.runMinuteTasks(ctx)
		}
	}
}

func (b *Bot) updatePresence(ctx context.Context) error {
	status, _ := b.podman.CollectStatuses(ctx, b.cfg.Maps)
	running := 0
	players := 0
	for _, st := range status {
		if !st.Running {
			continue
		}
		running++
		list, err := b.listPlayersLocked(ctx, st.Map)
		if err == nil {
			players += len(list)
		}
	}
	activity := "ARK 0/" + fmt.Sprintf("%d | Idle", b.cfg.Runtime.MaxRunningMaps)
	if running > 0 {
		activity = fmt.Sprintf("ARK %d/%d | Players %d", running, b.cfg.Runtime.MaxRunningMaps, players)
	}
	return b.dg.UpdateCustomStatus(activity)
}

func (b *Bot) runMinuteTasks(ctx context.Context) {
	now := mustJSTNow()
	b.runLocalBackupScheduler(ctx, now)
	b.runCloudBackupScheduler(ctx, now)
	b.runAnnouncementScheduler(ctx, now)
	b.runPreShutdownScheduler(ctx, now)
}

func (b *Bot) runLocalBackupScheduler(ctx context.Context, now time.Time) {
	if !b.cfg.Backup.Local.Enabled {
		return
	}
	status, _ := b.podman.CollectStatuses(ctx, b.cfg.Maps)
	for _, st := range status {
		if !st.Running || !mapBackupEnabled(st.Map) {
			continue
		}
		b.mu.Lock()
		next := b.nextLocalBackup[st.Map.MapID]
		if next.IsZero() {
			next = b.nextBackupFromStartLocked(st.Map.MapID, now)
			b.nextLocalBackup[st.Map.MapID] = next
		}
		b.mu.Unlock()
		if now.Before(next) {
			continue
		}
		lock := b.mapLock(st.Map.MapID)
		lock.Lock()
		errSave := b.rcon.SaveWorld(ctx, st.Map)
		time.Sleep(time.Duration(b.cfg.Runtime.SaveWaitSeconds) * time.Second)
		_, _, errBackup := b.backup.LocalBackup(ctx, st.Map)
		lock.Unlock()
		if errSave != nil {
			b.notify("ローカルバックアップ失敗: " + st.Map.DisplayName + " (saveworld失敗)")
		} else if errors.Is(errBackup, ErrBackupSkipped) {
			b.notify("ローカルバックアップスキップ: " + st.Map.DisplayName)
		} else if errBackup != nil {
			b.notify("ローカルバックアップ失敗: " + st.Map.DisplayName)
		} else {
			b.notify("ローカルバックアップ成功: " + st.Map.DisplayName)
		}
		b.mu.Lock()
		b.nextLocalBackup[st.Map.MapID] = b.nextBackupFromStartLocked(st.Map.MapID, now)
		b.mu.Unlock()
	}
}

func (b *Bot) runCloudBackupScheduler(ctx context.Context, now time.Time) {
	if !b.cfg.Backup.Cloud.Enabled {
		return
	}
	target, err := parseHHMM(b.cfg.Backup.Cloud.ScheduleHHMM, now)
	if err != nil {
		return
	}
	key := "cloud:" + target.Format("20060102-1504")
	b.mu.Lock()
	last := b.lastScheduleRun[key]
	b.mu.Unlock()
	if !now.After(target) || (!last.IsZero() && now.Sub(last) < time.Hour) {
		return
	}
	fails := []string{}
	oks := []string{}
	skips := []string{}
	for _, m := range b.cfg.Maps {
		if !mapBackupEnabled(m) {
			skips = append(skips, m.MapID)
			continue
		}
		if _, _, err := b.backup.UploadLatestToCloud(ctx, m); err != nil {
			if errors.Is(err, ErrBackupSkipped) || errors.Is(err, ErrLatestLocalNotFound) {
				skips = append(skips, m.MapID)
				continue
			}
			fails = append(fails, m.MapID)
		} else {
			oks = append(oks, m.MapID)
		}
	}
	if len(fails) > 0 {
		b.notify("クラウドバックアップ失敗: " + strings.Join(fails, ","))
	}
	if len(oks) > 0 {
		b.notify("クラウドバックアップ成功: " + strings.Join(oks, ","))
	}
	if len(skips) > 0 {
		b.notify("クラウドバックアップスキップ: " + strings.Join(skips, ","))
	}
	b.mu.Lock()
	b.lastScheduleRun[key] = now
	b.mu.Unlock()
}

func (b *Bot) runAnnouncementScheduler(ctx context.Context, now time.Time) {
	if !b.cfg.Announcements.Enabled {
		return
	}
	for _, item := range b.cfg.Announcements.Items {
		target, err := parseHHMM(item.Time, now)
		if err != nil {
			continue
		}
		if !weekdayMatch(item.Weekdays, now) {
			continue
		}
		key := "announce:" + item.Name + ":" + target.Format("20060102-1504")
		b.mu.Lock()
		last := b.lastScheduleRun[key]
		b.mu.Unlock()
		if !now.After(target) || (!last.IsZero() && now.Sub(last) < time.Hour) {
			continue
		}
		targetMaps := b.runningMapsFiltered(ctx, item.IncludeMaps)
		failed := []string{}
		for _, m := range targetMaps {
			if err := b.broadcastLocked(ctx, m, item.Message); err != nil {
				failed = append(failed, m.MapID)
			}
		}
		if len(failed) > 0 {
			b.notify(fmt.Sprintf("自動メッセージ失敗 %s: %s", item.Name, strings.Join(failed, ",")))
		} else {
			b.notify(fmt.Sprintf("自動メッセージ完了 %s (対象:%d)", item.Name, len(targetMaps)))
		}
		b.mu.Lock()
		b.lastScheduleRun[key] = now
		b.mu.Unlock()
	}
}

func (b *Bot) runPreShutdownScheduler(ctx context.Context, now time.Time) {
	if !b.cfg.PreShutdown.Enabled || b.cfg.PreShutdown.Action != "graceful_stop_all" || b.cfg.PreShutdown.Time == "" {
		return
	}
	target, err := parseHHMM(b.cfg.PreShutdown.Time, now)
	if err != nil {
		return
	}
	key := "preshutdown:" + target.Format("20060102-1504")
	b.mu.Lock()
	last := b.lastScheduleRun[key]
	b.mu.Unlock()
	if !now.After(target) || (!last.IsZero() && now.Sub(last) < time.Hour) {
		return
	}
	b.notify("pre_shutdown を開始します")
	if err := b.gracefulStopAll(ctx, "pre_shutdown"); err != nil {
		b.notify("pre_shutdown 失敗: " + err.Error())
	} else {
		b.notify("pre_shutdown 完了")
	}
	b.mu.Lock()
	b.lastScheduleRun[key] = now
	b.mu.Unlock()
}

func (b *Bot) runningMapsFiltered(ctx context.Context, include []string) []MapConfig {
	status, _ := b.podman.CollectStatuses(ctx, b.cfg.Maps)
	set := map[string]bool{}
	for _, id := range include {
		set[id] = true
	}
	out := []MapConfig{}
	for _, st := range status {
		if !st.Running {
			continue
		}
		if len(set) > 0 && !set[st.Map.MapID] {
			continue
		}
		out = append(out, st.Map)
	}
	return out
}

func (b *Bot) gracefulStopAll(ctx context.Context, reason string) error {
	status, _ := b.podman.CollectStatuses(ctx, b.cfg.Maps)
	failed := []string{}
	for _, st := range status {
		if !st.Running {
			continue
		}
		if _, err := b.cmdStop(ctx, st.Map.MapID, true); err != nil {
			failed = append(failed, st.Map.MapID)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%s 失敗: %s", reason, strings.Join(failed, ","))
	}
	return nil
}

func (b *Bot) restoreRuntimeState(ctx context.Context) {
	status, err := b.podman.CollectStatuses(ctx, b.cfg.Maps)
	if err != nil {
		b.log.Warn("起動時状態復元の収集中エラー: %v", err)
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, st := range status {
		if !st.Running {
			continue
		}
		ins, ierr := b.podman.InspectState(ctx, st.Map.MapID)
		if ierr != nil {
			b.log.Warn("起動時状態復元 inspect失敗: map=%s err=%v", st.Map.MapID, ierr)
			continue
		}
		started := ins.StartedAt
		if started.IsZero() {
			started = now
		}
		b.startedAt[st.Map.MapID] = started
		if b.cfg.Backup.Local.Enabled {
			b.nextLocalBackup[st.Map.MapID] = b.nextBackupFromStartNoLock(started, now)
		}
	}
}

func (b *Bot) nextBackupFromStartNoLock(started, now time.Time) time.Time {
	interval := time.Duration(b.cfg.Backup.Local.IntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 120 * time.Minute
	}
	if now.Before(started) || now.Equal(started) {
		return started.Add(interval)
	}
	passed := now.Sub(started)
	steps := int64(passed / interval)
	return started.Add(time.Duration(steps+1) * interval)
}

func (b *Bot) nextBackupFromStartLocked(mapID string, now time.Time) time.Time {
	started := b.startedAt[mapID]
	if started.IsZero() {
		started = now
		b.startedAt[mapID] = started
	}
	return b.nextBackupFromStartNoLock(started, now)
}

func (b *Bot) sortedMapStates(ctx context.Context) ([]MapStatus, error) {
	status, err := b.podman.CollectStatuses(ctx, b.cfg.Maps)
	sort.Slice(status, func(i, j int) bool { return status[i].Map.MapID < status[j].Map.MapID })
	return status, err
}

func joinErrs(errs ...error) error {
	filtered := make([]error, 0, len(errs))
	for _, e := range errs {
		if e != nil {
			filtered = append(filtered, e)
		}
	}
	return errors.Join(filtered...)
}

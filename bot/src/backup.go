package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	awsCfg "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type BackupManager struct {
	cfg *Config
	pod *PodmanClient
}

var (
	ErrBackupSkipped        = errors.New("backup skipped")
	ErrLatestLocalNotFound  = errors.New("latest local backup not found")
)

func NewBackupManager(cfg *Config, pod *PodmanClient) *BackupManager {
	return &BackupManager{cfg: cfg, pod: pod}
}

func mapBackupEnabled(m MapConfig) bool {
	if m.BackupEnabled == nil {
		return true
	}
	return *m.BackupEnabled
}

func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		total += fi.Size()
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return total, err
}

func (b *BackupManager) saveDir(m MapConfig) string {
	if strings.TrimSpace(m.SaveDirOverride) != "" {
		return m.SaveDirOverride
	}
	return b.pod.DefaultSaveDir(m.MapID)
}

func (b *BackupManager) LocalBackup(ctx context.Context, m MapConfig) (string, int64, error) {
	_ = ctx
	if !b.cfg.Backup.Local.Enabled || !mapBackupEnabled(m) {
		return "", 0, ErrBackupSkipped
	}
	baseDir := filepath.Join(b.cfg.Backup.Local.OutDir, m.MapID)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return "", 0, err
	}
	ts := time.Now().Format("20060102-150405")
	outPath := filepath.Join(baseDir, fmt.Sprintf("%s-%s.tar.gz", m.MapID, ts))

	src := b.saveDir(m)
	f, err := os.Create(outPath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	err = filepath.Walk(src, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer in.Close()
			if _, err := io.Copy(tw, in); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", 0, err
	}

	stat, err := os.Stat(outPath)
	if err != nil {
		return "", 0, err
	}
	if err := b.keepLocalGenerations(baseDir); err != nil {
		return outPath, stat.Size(), err
	}
	return outPath, stat.Size(), nil
}

func (b *BackupManager) keepLocalGenerations(baseDir string) error {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return err
	}
	files := make([]string, 0)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		files = append(files, filepath.Join(baseDir, e.Name()))
	}
	sort.Strings(files)
	keep := b.cfg.Backup.Local.KeepGenerations
	if keep < 1 {
		keep = 1
	}
	for len(files) > keep {
		if err := os.Remove(files[0]); err != nil {
			return err
		}
		files = files[1:]
	}
	return nil
}

func (b *BackupManager) latestLocalBackup(m MapConfig) (string, int64, error) {
	baseDir := filepath.Join(b.cfg.Backup.Local.OutDir, m.MapID)
	entries, err := os.ReadDir(baseDir)
	if os.IsNotExist(err) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	files := make([]string, 0)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		files = append(files, filepath.Join(baseDir, e.Name()))
	}
	if len(files) == 0 {
		return "", 0, nil
	}
	sort.Strings(files)
	latest := files[len(files)-1]
	st, err := os.Stat(latest)
	if err != nil {
		return "", 0, err
	}
	return latest, st.Size(), nil
}

func (b *BackupManager) cloudClient(ctx context.Context) (*s3.Client, error) {
	if !b.cfg.Backup.Cloud.Enabled {
		return nil, nil
	}
	endpoint := getEnv("R2_ENDPOINT", "")
	ak := getEnv("R2_ACCESS_KEY_ID", "")
	sk := getEnv("R2_SECRET_ACCESS_KEY", "")
	region := getEnv("R2_REGION", "auto")
	if endpoint == "" || ak == "" || sk == "" {
		return nil, fmt.Errorf("R2環境変数不足")
	}
	awsConf, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(ak, sk, "")),
	)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(awsConf, func(o *s3.Options) {
		o.BaseEndpoint = awsCfg.String(endpoint)
		o.UsePathStyle = true
	}), nil
}

func (b *BackupManager) UploadLatestToCloud(ctx context.Context, m MapConfig) (string, int64, error) {
	if !b.cfg.Backup.Cloud.Enabled || !mapBackupEnabled(m) {
		return "", 0, ErrBackupSkipped
	}
	latestPath, size, err := b.latestLocalBackup(m)
	if err != nil {
		return "", size, err
	}
	if latestPath == "" {
		return "", 0, ErrLatestLocalNotFound
	}
	client, err := b.cloudClient(ctx)
	if err != nil {
		return "", 0, err
	}
	key := strings.TrimSuffix(strings.TrimPrefix(filepath.ToSlash(filepath.Join(b.cfg.Backup.Cloud.Prefix, m.MapID, filepath.Base(latestPath))), "/"), "")
	in, err := os.Open(latestPath)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &b.cfg.Backup.Cloud.Bucket,
		Key:    &key,
		Body:   in,
	})
	if err != nil {
		return "", 0, err
	}
	if err := b.cleanupCloudOld(ctx, client, m); err != nil {
		return key, size, err
	}
	return key, size, nil
}

func (b *BackupManager) cleanupCloudOld(ctx context.Context, client *s3.Client, m MapConfig) error {
	if b.cfg.Backup.Cloud.KeepDays <= 0 {
		return nil
	}
	prefix := filepath.ToSlash(filepath.Join(b.cfg.Backup.Cloud.Prefix, m.MapID)) + "/"
	resp, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &b.cfg.Backup.Cloud.Bucket, Prefix: &prefix})
	if err != nil {
		return err
	}
	cutoff := time.Now().AddDate(0, 0, -b.cfg.Backup.Cloud.KeepDays)
	for _, obj := range resp.Contents {
		if obj.LastModified == nil || obj.Key == nil {
			continue
		}
		if obj.LastModified.After(cutoff) {
			continue
		}
		_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &b.cfg.Backup.Cloud.Bucket, Key: obj.Key})
		if err != nil {
			return err
		}
	}
	return nil
}

func (b *BackupManager) LatestCloudSize(ctx context.Context, m MapConfig) (int64, error) {
	if !b.cfg.Backup.Cloud.Enabled {
		return 0, nil
	}
	client, err := b.cloudClient(ctx)
	if err != nil {
		return 0, err
	}
	prefix := filepath.ToSlash(filepath.Join(b.cfg.Backup.Cloud.Prefix, m.MapID)) + "/"
	resp, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &b.cfg.Backup.Cloud.Bucket, Prefix: &prefix})
	if err != nil {
		return 0, err
	}
	var latest time.Time
	var size int64
	for _, obj := range resp.Contents {
		if obj.LastModified == nil || obj.Size == nil {
			continue
		}
		if obj.LastModified.After(latest) {
			latest = *obj.LastModified
			size = *obj.Size
		}
	}
	return size, nil
}

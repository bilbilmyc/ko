package etcd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ko-build/ko/internal/exec"
	"github.com/ko-build/ko/internal/logger"
)

// BackupService manages on-host etcd snapshots. It deploys:
//   - /etc/systemd/system/ko-etcd-backup.service   — one-shot
//   - /etc/systemd/system/ko-etcd-backup.timer     — OnCalendar=*:0/8 + Persistent=true
// and a small shell script at /usr/local/bin/ko-etcd-backup that runs
// the snapshot + pruning.
type BackupService struct {
	Exec       exec.Executor
	PKIDir     string // default /etc/etcd/pki
	BackupDir  string // default /var/backups/etcd
	RetainDays int    // default 14
	Interval   string // OnCalendar spec, default "*-*-* *:00/8:00"  (every 8h, on the hour)
}

func NewBackupService(ex exec.Executor) *BackupService {
	return &BackupService{
		Exec:       ex,
		PKIDir:     "/etc/etcd/pki",
		BackupDir:  "/var/backups/etcd",
		RetainDays: 14,
		Interval:   "*-*-* *:00/8:00",
	}
}

// Install lays down the script + service + timer and enables the timer.
// Idempotent: re-running overwrites the unit files and re-enables the
// timer with the new schedule.
func (b *BackupService) Install(ctx context.Context, host string) error {
	script := b.scriptBody()
	scriptPath, err := writeTempFile("ko-etcd-backup", script)
	if err != nil {
		return err
	}
	defer os.Remove(scriptPath)
	if err := b.Exec.Scp(ctx, host, scriptPath, "/usr/local/bin/ko-etcd-backup"); err != nil {
		return fmt.Errorf("scp script: %w", err)
	}
	if r := b.Exec.Run(ctx, host, "chmod 0755 /usr/local/bin/ko-etcd-backup"); r.Failed() {
		return fmt.Errorf("chmod script: %w", r.Err)
	}

	svc := b.serviceUnitBody()
	svcPath, err := writeTempFile("ko-etcd-backup.service", svc)
	if err != nil {
		return err
	}
	defer os.Remove(svcPath)
	if err := b.Exec.Scp(ctx, host, svcPath, "/etc/systemd/system/ko-etcd-backup.service"); err != nil {
		return fmt.Errorf("scp service: %w", err)
	}

	tmr := b.timerUnitBody()
	tmrPath, err := writeTempFile("ko-etcd-backup.timer", tmr)
	if err != nil {
		return err
	}
	defer os.Remove(tmrPath)
	if err := b.Exec.Scp(ctx, host, tmrPath, "/etc/systemd/system/ko-etcd-backup.timer"); err != nil {
		return fmt.Errorf("scp timer: %w", err)
	}

	// mkdir backup dir + chmod 0700 (backups may include certs if we ever inline them)
	cmds := []string{
		fmt.Sprintf("mkdir -p %s", b.BackupDir),
		fmt.Sprintf("chmod 0700 %s", b.BackupDir),
		"systemctl daemon-reload",
		"systemctl enable --now ko-etcd-backup.timer",
	}
	for _, c := range cmds {
		if r := b.Exec.Run(ctx, host, c); r.Failed() {
			return fmt.Errorf("%q: %w", c, r.Err)
		}
	}
	logger.Info("etcd backup: installed (timer + service)", "host", host, "interval", b.Interval)
	return nil
}

// Uninstall stops the timer, removes the units + script. Backups in
// BackupDir are LEFT IN PLACE — operator decides.
func (b *BackupService) Uninstall(ctx context.Context, host string) error {
	cmds := []string{
		"systemctl disable --now ko-etcd-backup.timer 2>/dev/null || true",
		"systemctl stop ko-etcd-backup.service 2>/dev/null || true",
		"rm -f /etc/systemd/system/ko-etcd-backup.service /etc/systemd/system/ko-etcd-backup.timer /usr/local/bin/ko-etcd-backup",
		"systemctl daemon-reload",
		"systemctl reset-failed ko-etcd-backup.service 2>/dev/null || true",
		"systemctl reset-failed ko-etcd-backup.timer 2>/dev/null || true",
	}
	for _, c := range cmds {
		if r := b.Exec.Run(ctx, host, c); r.Failed() {
			return fmt.Errorf("%q: %w", c, r.Err)
		}
	}
	return nil
}

// Snapshot is the manual on-demand snapshot path. Returns the local
// path of the file copied off the host.
func (b *BackupService) Snapshot(ctx context.Context, host, memberName string) (string, error) {
	ts := time.Now().UTC().Format("20060102-150405")
	remote := fmt.Sprintf("/var/backups/etcd/%s-%s.db", memberName, ts)
	script := fmt.Sprintf(`set -euo pipefail
mkdir -p /var/backups/etcd
ETCDCTL_API=3 etcdctl --endpoints=https://127.0.0.1:2379 \
  --cacert=%s/ca.crt \
  --cert=%s/client.crt \
  --key=%s/client.key \
  snapshot save %s
chmod 0600 %s
`, b.PKIDir, b.PKIDir, b.PKIDir, remote, remote)
	if r := b.Exec.Run(ctx, host, script); r.Failed() {
		return "", fmt.Errorf("etcdctl snapshot: %w", r.Err)
	}
	local := fmt.Sprintf("ko-etcd-%s-%s.db", memberName, ts)
	if err := b.Exec.Scp(ctx, host, remote, local); err != nil {
		return "", fmt.Errorf("scp snapshot: %w", err)
	}
	logger.Info("etcd snapshot saved", "host", host, "name", memberName, "local", local)
	return local, nil
}

// BackupInfo describes one snapshot on the host.
type BackupInfo struct {
	Host     string
	Name     string // member name
	Filename string
	Size     int64
	ModTime  time.Time
}

// backupFilenameRE matches the trailing -YYYYMMDD-HHMMSS timestamp we
// append in scriptBody. The member name is everything before it. The
// timestamp is 15 chars ("-YYYYMMDD-HHMMSS"), so we strip that off the
// stem to recover the bare member name.
var backupFilenameRE = regexp.MustCompile(`-\d{8}-\d{6}$`)

// ListBackups reads /var/backups/etcd/ on the given host and returns
// sorted-by-mtime-desc backups.
func (b *BackupService) ListBackups(ctx context.Context, host string) ([]BackupInfo, error) {
	script := fmt.Sprintf(`find %s -maxdepth 1 -type f -name '*.db' -printf '%%f %%s %%T@\n' 2>/dev/null | sort -k3 -nr`, b.BackupDir)
	r := b.Exec.Run(ctx, host, script)
	if r.Failed() {
		return nil, fmt.Errorf("list backups: %w", r.Err)
	}
	var out []BackupInfo
	for _, line := range strings.Split(strings.TrimRight(string(r.Stdout), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 3 {
			continue
		}
		var size int64
		fmt.Sscanf(parts[1], "%d", &size)
		var ts float64
		fmt.Sscanf(parts[2], "%f", &ts)
		sec := int64(ts)
		stem := strings.TrimSuffix(parts[0], ".db")
		// stem = "<member>-YYYYMMDD-HHMMSS" — strip the timestamp suffix
		// to recover the bare member name.
		memberName := backupFilenameRE.ReplaceAllString(stem, "")
		out = append(out, BackupInfo{
			Host:     host,
			Name:     memberName,
			Filename: parts[0],
			Size:     size,
			ModTime:  time.Unix(sec, 0),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// scriptBody is the on-host shell script that takes + prunes snapshots.
// Kept inline (not on disk) so the unit files are self-contained and the
// operator can `cat /usr/local/bin/ko-etcd-backup` to see exactly what
// runs every 8 hours.
func (b *BackupService) scriptBody() string {
	return fmt.Sprintf(`#!/usr/bin/env bash
# ko-managed etcd snapshot. Invoked by ko-etcd-backup.timer every 8h.
set -euo pipefail

HOSTNAME_SHORT=$(hostname -s 2>/dev/null || hostname)
TS=$(date -u +%%Y%%m%%d-%%H%%M%%S)
BACKUP_DIR=%s
RETENTION_DAYS=%d
PKI_DIR=%s

mkdir -p "$BACKUP_DIR"
TARGET="$BACKUP_DIR/${HOSTNAME_SHORT}-${TS}.db"

# Take snapshot via loopback client. Member name auto-resolved by etcd.
ETCDCTL_API=3 etcdctl --endpoints=https://127.0.0.1:2379 \
  --cacert=$PKI_DIR/ca.crt \
  --cert=$PKI_DIR/client.crt \
  --key=$PKI_DIR/client.key \
  snapshot save "$TARGET"

chmod 0600 "$TARGET"

# Prune: keep RETENTION_DAYS worth, drop older.
find "$BACKUP_DIR" -maxdepth 1 -type f -name '*.db' -mtime +$RETENTION_DAYS -delete
`, b.BackupDir, b.RetainDays, b.PKIDir)
}

func (b *BackupService) serviceUnitBody() string {
	return `[Unit]
Description=ko: etcd snapshot
Requires=etcd.service
After=etcd.service

[Service]
Type=oneshot
ExecStart=/usr/local/bin/ko-etcd-backup
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7
`
}

func (b *BackupService) timerUnitBody() string {
	return fmt.Sprintf(`[Unit]
Description=ko: etcd snapshot (every %s)

[Timer]
OnCalendar=%s
Persistent=true
AccuracySec=60s
RandomizedDelaySec=5min

[Install]
WantedBy=timers.target
`, parseHumanInterval(b.Interval), b.Interval)
}

// parseHumanInterval best-efforts to render the interval in plain English
// for the timer's Description. Falls back to the raw spec on parse fail.
func parseHumanInterval(spec string) string {
	// We only support a few well-known cadences to keep this readable.
	switch spec {
	case "*-*-* *:00/8:00":
		return "8h"
	case "*-*-* *:00/4:00":
		return "4h"
	case "*-*-* *:00/2:00":
		return "2h"
	case "daily":
		return "24h"
	case "hourly":
		return "1h"
	}
	return spec
}

// ensure imports of bytes / filepath stay for the package's other files
var _ = bytes.NewBuffer
var _ = filepath.Join

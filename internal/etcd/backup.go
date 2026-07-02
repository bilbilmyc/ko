package etcd

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
	// RestoreHealthTimeout bounds the post-restore /health probe on the
	// restored member. Zero means the default (30s).
	RestoreHealthTimeout time.Duration
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

// RestoreOptions describes one member's restore from a snapshot file.
// SnapshotPath is the LOCAL path; we scp it onto the member host. The
// InitialCluster string must include every member of the cluster, in
// `name=peerURL` format, comma-separated.
type RestoreOptions struct {
	Member         Member
	SnapshotPath   string // local snapshot file
	InitialCluster string // "m1=https://10.0.0.31:2380,m2=..."
}

// Restore puts a single member back from a snapshot. Steps:
//  1. validate the snapshot file is readable locally
//  2. stop etcd.service on the member
//  3. move current data-dir to .broken-<ts> (so an operator can recover)
//  4. scp snapshot to /tmp on the host
//  5. etcdctl snapshot restore into a fresh data-dir
//  6. start etcd.service
//  7. wait for /health to come back
//
// Caller is expected to restore all members of the cluster in sequence —
// etcd 3.5 requires the full --initial-cluster list on every restore, but
// only one member is up at a time during this operation (we restart each
// in isolation).
func (b *BackupService) Restore(ctx context.Context, opts RestoreOptions) error {
	if opts.SnapshotPath == "" {
		return fmt.Errorf("snapshot path required")
	}
	if _, err := os.Stat(opts.SnapshotPath); err != nil {
		return fmt.Errorf("snapshot %q: %w", opts.SnapshotPath, err)
	}
	if opts.Member.Name == "" {
		return fmt.Errorf("member name required")
	}
	if opts.Member.Host == "" {
		return fmt.Errorf("member host required")
	}
	if opts.Member.DataDir == "" {
		opts.Member.DataDir = "/var/lib/etcd/" + opts.Member.Name
	}
	if opts.Member.InitialPeerURLs == "" {
		opts.Member.InitialPeerURLs = fmt.Sprintf("https://%s:2380", opts.Member.Host)
	}
	if opts.InitialCluster == "" {
		return fmt.Errorf("--initial-cluster required (run from a config-aware caller)")
	}

	host := opts.Member.Host
	ts := time.Now().UTC().Format("20060102-150405")
	logger.Info("etcd restore: starting", "host", host, "name", opts.Member.Name, "snapshot", opts.SnapshotPath)

	// 1. Stop etcd.service. Idempotent — already-stopped returns non-zero, which we tolerate.
	if r := b.Exec.Run(ctx, host, "systemctl stop etcd.service 2>/dev/null || true"); r.Failed() {
		return fmt.Errorf("stop etcd: %w", r.Err)
	}

	// 2. Move the existing data-dir aside. If nothing is there yet (fresh
	//    member), the mv fails — tolerate.
	mv := fmt.Sprintf("test -e %s && mv %s %s.broken-%s || true", opts.Member.DataDir, opts.Member.DataDir, opts.Member.DataDir, ts)
	if r := b.Exec.Run(ctx, host, mv); r.Failed() {
		return fmt.Errorf("move data-dir aside: %w", r.Err)
	}

	// 3. Fresh data-dir.
	if r := b.Exec.Run(ctx, host, fmt.Sprintf("mkdir -p %s", opts.Member.DataDir)); r.Failed() {
		return fmt.Errorf("mkdir data-dir: %w", r.Err)
	}

	// 4. scp snapshot onto the host.
	remoteSnap := fmt.Sprintf("/tmp/ko-etcd-restore-%s.db", ts)
	if err := b.Exec.Scp(ctx, host, opts.SnapshotPath, remoteSnap); err != nil {
		return fmt.Errorf("scp snapshot: %w", err)
	}

	// 5. Run etcdctl snapshot restore. Use the on-host etcdctl (installed
	//    alongside etcd by Service.Install).
	restore := fmt.Sprintf(`set -euo pipefail
ETCDCTL_API=3 /usr/local/bin/etcdctl snapshot restore %s \
  --name=%s \
  --initial-cluster=%s \
  --initial-advertise-peer-urls=%s \
  --data-dir=%s
chmod 0700 %s
`,
		remoteSnap,
		opts.Member.Name,
		opts.InitialCluster,
		opts.Member.InitialPeerURLs,
		opts.Member.DataDir,
		opts.Member.DataDir,
	)
	if r := b.Exec.Run(ctx, host, restore); r.Failed() {
		return fmt.Errorf("etcdctl snapshot restore: %w", r.Err)
	}

	// 6. Clean up the staged snapshot file.
	_ = b.Exec.Run(ctx, host, fmt.Sprintf("rm -f %s", remoteSnap))

	// 7. Start etcd.service.
	if r := b.Exec.Run(ctx, host, "systemctl start etcd.service"); r.Failed() {
		return fmt.Errorf("start etcd: %w", r.Err)
	}

	// 8. Wait for /health (up to ~30s, generous because of WAL replay on cold start).
	healthTimeout := b.RestoreHealthTimeout
	if healthTimeout == 0 {
		healthTimeout = 30 * time.Second
	}
	deadline := time.Now().Add(healthTimeout)
	for time.Now().Before(deadline) {
		probe := fmt.Sprintf(
			`curl -sk --max-time 3 --cacert %s/ca.crt --cert %s/client.crt --key %s/client.key https://127.0.0.1:2379/health 2>/dev/null`,
			b.PKIDir, b.PKIDir, b.PKIDir)
		r := b.Exec.Run(ctx, host, probe)
		if !r.Failed() && bytes.Contains(r.Stdout, []byte(`"health":"true"`)) {
			logger.Info("etcd restore: healthy", "host", host, "name", opts.Member.Name)
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("etcd on %s did not become healthy within %s after restore", host, healthTimeout)
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

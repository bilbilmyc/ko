package etcd

import (
	"context"
	"strings"
	"testing"

	"github.com/ko-build/ko/internal/exec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackupService_ScriptBody_HasExpectedParts(t *testing.T) {
	bs := NewBackupService(&mockExecutor{})
	body := bs.scriptBody()
	for _, want := range []string{
		"#!/usr/bin/env bash",
		"set -euo pipefail",
		"ETCDCTL_API=3",
		"etcdctl",
		"snapshot save",
		"BACKUP_DIR=/var/backups/etcd",
		`TARGET="$BACKUP_DIR/${HOSTNAME_SHORT}-${TS}.db"`,
		`-mtime +$RETENTION_DAYS -delete`,
		"RETENTION_DAYS=14",
	} {
		assert.Contains(t, body, want, "missing in script: %q", want)
	}
}

func TestBackupService_ScriptBody_DateFormatIntact(t *testing.T) {
	bs := NewBackupService(&mockExecutor{})
	body := bs.scriptBody()
	assert.Contains(t, body, "date -u +%Y%m%d-%H%M%S",
		"date format must survive — the %% escapes are for shell, not Go fmt")
}

func TestBackupService_ScriptBody_ConfigurableRetention(t *testing.T) {
	bs := NewBackupService(&mockExecutor{})
	bs.RetainDays = 7
	body := bs.scriptBody()
	assert.Contains(t, body, "RETENTION_DAYS=7",
		"retention days should appear as a tunable variable, not a literal")
	assert.Contains(t, body, "-mtime +$RETENTION_DAYS")
}

func TestBackupService_ServiceUnit(t *testing.T) {
	bs := NewBackupService(&mockExecutor{})
	u := bs.serviceUnitBody()
	assert.Contains(t, u, "Type=oneshot")
	assert.Contains(t, u, "Requires=etcd.service")
	assert.Contains(t, u, "After=etcd.service")
	assert.Contains(t, u, "ExecStart=/usr/local/bin/ko-etcd-backup")
}

func TestBackupService_TimerUnit_DefaultInterval(t *testing.T) {
	bs := NewBackupService(&mockExecutor{})
	u := bs.timerUnitBody()
	assert.Contains(t, u, "OnCalendar=*-*-* *:00/8:00")
	assert.Contains(t, u, "Persistent=true")
	assert.Contains(t, u, "every 8h",
		"description should render human-readable interval for the operator")
}

func TestBackupService_TimerUnit_FallbackForUnknownInterval(t *testing.T) {
	bs := NewBackupService(&mockExecutor{})
	bs.Interval = "*:*:00/45" // not in the lookup table
	u := bs.timerUnitBody()
	assert.Contains(t, u, "OnCalendar=*:*:00/45")
	assert.Contains(t, u, "every *:*:00/45", "unknown intervals fall back to raw spec")
}

func TestListBackups_ParsesAndSorts(t *testing.T) {
	m := &mockExecutor{}
	m.RunFn = func(_ context.Context, host, command string) exec.Result {
		if !strings.HasPrefix(command, "find") {
			return exec.Result{Err: nil}
		}
		// Two snapshots, out of order
		return exec.Result{Stdout: []byte(
			"etcd-1-20260101-120000.db 12345 1735732800\n" +
				"etcd-1-20260101-200000.db 67890 1735754400\n")}
	}
	bs := NewBackupService(m)
	bs.BackupDir = "/var/backups/etcd"
	got, err := bs.ListBackups(context.Background(), "h1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	// Sorted by mtime desc
	assert.Equal(t, int64(67890), got[0].Size)
	assert.Equal(t, int64(12345), got[1].Size)
	assert.Equal(t, "etcd-1", got[0].Name)
	assert.Equal(t, "etcd-1", got[1].Name)
}

func TestListBackups_EmptyOutput(t *testing.T) {
	m := &mockExecutor{}
	m.RunFn = func(_ context.Context, _, _ string) exec.Result {
		return exec.Result{Stdout: []byte("")}
	}
	bs := NewBackupService(m)
	got, err := bs.ListBackups(context.Background(), "h1")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListBackups_BadOutputIsSkipped(t *testing.T) {
	m := &mockExecutor{}
	m.RunFn = func(_ context.Context, _, _ string) exec.Result {
		return exec.Result{Stdout: []byte("garbage line\netcd-1-20260101-120000.db 100 1700000000\n")}
	}
	bs := NewBackupService(m)
	got, err := bs.ListBackups(context.Background(), "h1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "etcd-1", got[0].Name)
}

func TestParseHumanInterval_KnownSpecs(t *testing.T) {
	cases := map[string]string{
		"*-*-* *:00/8:00": "8h",
		"*-*-* *:00/4:00": "4h",
		"daily":           "24h",
		"hourly":          "1h",
		"*-*-* *:00/2:00": "2h",
	}
	for in, want := range cases {
		assert.Equal(t, want, parseHumanInterval(in))
	}
}

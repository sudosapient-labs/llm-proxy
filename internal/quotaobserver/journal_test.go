package quotaobserver

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quotaforecast"
)

func journalRecord(at time.Time, used float64) record {
	return record{Pool: "synthetic-pool", Model: "synthetic-model", Report: quotaforecast.Report{
		Measured: quotaforecast.Sample{At: at, Accounts: []quotaforecast.Account{{
			ID: "A", ObservedAt: at, Healthy: true,
			FiveHour: testWindow(used, epoch.Add(5*time.Hour)), Weekly: testWindow(.4, epoch.Add(7*24*time.Hour)),
		}}},
	}}
}

func TestJournalRotationRecoveryAndPrivacy(t *testing.T) {
	j := journal{path: filepath.Join(t.TempDir(), "private", "observations.jsonl"), maxBytes: 3000, files: 3}
	for i := 0; i < 30; i++ {
		if err := j.append(journalRecord(epoch.Add(time.Duration(i)*time.Minute), .2+float64(i)/100)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < j.files; i++ {
		info, err := os.Stat(j.file(i))
		if err != nil || info.Size() > j.maxBytes || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
			t.Fatalf("journal bounds/permissions violated: %v, %v", info, err)
		}
	}
	if _, err := os.Stat(j.file(j.files)); !os.IsNotExist(err) {
		t.Fatal("rotation retained more files than configured")
	}
	info, _ := os.Stat(filepath.Dir(j.path))
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0700 {
		t.Fatal("new journal directory is not private")
	}
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(`{"incomplete":`); err != nil {
		t.Fatal(err)
	}
	closeJournalFile(f)
	seeds, err := j.restore("synthetic-pool", epoch.Add(30*time.Minute))
	if err != nil || len(seeds) != 2 {
		t.Fatalf("recovery failed: %v", err)
	}
	o, err := quotaforecast.New(testConfig().Assumptions)
	if err != nil {
		t.Fatal(err)
	}
	var report quotaforecast.Report
	for _, seed := range seeds {
		report, err = o.Observe(seed)
		if err != nil {
			t.Fatal(err)
		}
	}
	if rate := report.Forecasts[0].FiveHour.BurnPerHour; rate == nil || *rate < .599 || *rate > .601 {
		t.Fatalf("restart did not recover the two distinct measurements: %+v", report.Forecasts)
	}
	other, err := j.restore("changed-mapping", epoch.Add(30*time.Minute))
	if err != nil || len(other) != 0 {
		t.Fatal("restoration crossed changed pool mapping")
	}
	j.files = 1
	if err := j.append(journalRecord(epoch.Add(31*time.Minute), .5)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 3; i++ {
		if _, err := os.Stat(j.file(i)); !os.IsNotExist(err) {
			t.Fatal("lower retention kept old journal files")
		}
	}
}

func TestJournalRejectsUnsafeFilesAndOversizeRecords(t *testing.T) {
	j := journal{path: filepath.Join(t.TempDir(), "observations.jsonl"), maxBytes: 1, files: 1}
	if err := j.append(journalRecord(epoch, .2)); err == nil {
		t.Fatal("oversize record accepted")
	}
	j.maxBytes = 3000
	if err := os.Symlink(filepath.Join(t.TempDir(), "other.jsonl"), j.path); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires Windows privileges")
		}
		t.Fatal(err)
	}
	if err := j.append(journalRecord(epoch, .2)); err == nil {
		t.Fatal("symlink accepted for writes")
	}
	if _, err := j.restore("synthetic-pool", epoch); err == nil {
		t.Fatal("symlink accepted for restore")
	}
}

func TestJournalAppendAfterInterruptedWriteRecoversNewObservation(t *testing.T) {
	j := journal{path: filepath.Join(t.TempDir(), "observations.jsonl"), maxBytes: 3000, files: 2}
	if err := j.append(journalRecord(epoch, .2)); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(`{"interrupted":`); err != nil {
		t.Fatal(err)
	}
	closeJournalFile(f)
	latest := epoch.Add(time.Minute)
	if err = j.append(journalRecord(latest, .3)); err != nil {
		t.Fatal(err)
	}
	seeds, err := j.restore("synthetic-pool", latest)
	if err != nil || len(seeds) != 2 {
		t.Fatalf("restore failed: %v", err)
	}
	if !seeds[1].Accounts[0].ObservedAt.Equal(latest) {
		t.Fatal("interrupted tail swallowed the next observation")
	}
}

func TestJournalReloadEnforcesSmallerBoundsAndPrivateBackups(t *testing.T) {
	j := journal{path: filepath.Join(t.TempDir(), "observations.jsonl"), maxBytes: 3000, files: 3}
	for _, index := range []int{0, 1} {
		if err := os.WriteFile(j.file(index), make([]byte, 4000), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(j.file(2), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(j.file(2), 0644); err != nil {
		t.Fatal(err)
	}
	if err := j.append(journalRecord(epoch, .2)); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < j.files; index++ {
		info, err := os.Stat(j.file(index))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || info.Size() > j.maxBytes || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
			t.Fatalf("reload retained oversized or public history: %v, %v", info, err)
		}
	}
	if seeds, err := j.restore("synthetic-pool", epoch); err != nil || len(seeds) != 2 {
		t.Fatalf("oversized old journal prevented new baseline restoration: %v", err)
	}
}

func TestControllerRestoresOnlyMatchingCredentialPool(t *testing.T) {
	cfg, m, clock := testConfig(), newTestManager(), &testClock{}
	path := filepath.Join(t.TempDir(), "observations.jsonl")
	c, err := New(cfg, m, path, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		r := journalRecord(clock.Now(), .2+float64(i)*.1)
		r.Pool = c.pool
		if err = c.journal.append(r); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Minute)
	}
	restarted, err := New(cfg, m, path, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	status, err := restarted.Snapshot("", "")
	if err != nil || status.Report.Forecasts[0].FiveHour.BurnPerHour == nil {
		t.Fatal("controller failed to restore rates")
	}
	cfg.Accounts[0].AuthID = "replacement-credential"
	replaced, err := New(cfg, m, path, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	status, err = replaced.Snapshot("", "")
	if err != nil || status.Report.Forecasts[0].FiveHour.State != "missing" {
		t.Fatal("history crossed private credential mapping")
	}
}

package quotaobserver

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quotaforecast"
	log "github.com/sirupsen/logrus"
)

type journal struct {
	path     string
	maxBytes int64
	files    int
}

type record struct {
	Pool       string               `json:"pool"`
	Model      string               `json:"model"`
	Report     quotaforecast.Report `json:"report"`
	Collection []CollectionStatus   `json:"collection"`
}

func (j journal) file(index int) string {
	if index == 0 {
		return j.path
	}
	return fmt.Sprintf("%s.%d", j.path, index)
}

func closeJournalFile(f *os.File) {
	if errClose := f.Close(); errClose != nil {
		log.Warn("failed to close quota forecast journal")
	}
}

func (j journal) append(r record) error {
	data, errMarshal := json.Marshal(r)
	if errMarshal != nil {
		return errors.New("quota forecast journal encoding failed")
	}
	data = append(data, '\n')
	if int64(len(data)) > j.maxBytes {
		return errors.New("quota forecast report exceeds journal bound")
	}
	if errMkdir := os.MkdirAll(filepath.Dir(j.path), 0700); errMkdir != nil {
		return errors.New("quota forecast journal directory unavailable")
	}
	// A reload may lower retention. Remove only our bounded numbered siblings.
	for index := j.files; index < 10; index++ {
		if errRemove := os.Remove(j.file(index)); errRemove != nil && !os.IsNotExist(errRemove) {
			return errors.New("quota forecast journal retention cleanup failed")
		}
	}
	// Apply privacy and size bounds to every retained file, including backups
	// created under older settings. Oversize history cannot be restored.
	for index := 0; index < j.files; index++ {
		path := j.file(index)
		info, errStat := os.Lstat(path)
		if os.IsNotExist(errStat) {
			continue
		}
		if errStat != nil || !info.Mode().IsRegular() {
			return errors.New("quota forecast journal must be a regular file")
		}
		if info.Size() > j.maxBytes {
			if errRemove := os.Remove(path); errRemove != nil {
				return errors.New("quota forecast journal retention cleanup failed")
			}
		} else if errChmod := os.Chmod(path, 0600); errChmod != nil {
			return errors.New("quota forecast journal permissions failed")
		}
	}
	if info, errStat := os.Lstat(j.path); errStat == nil {
		if !info.Mode().IsRegular() {
			return errors.New("quota forecast journal must be a regular file")
		}
		// Reserve one byte to separate a possible interrupted trailing record.
		if info.Size()+int64(len(data))+1 > j.maxBytes {
			if errRemove := os.Remove(j.file(j.files - 1)); errRemove != nil && !os.IsNotExist(errRemove) {
				return errors.New("quota forecast journal rotation failed")
			}
			for index := j.files - 2; index >= 0; index-- {
				if errRename := os.Rename(j.file(index), j.file(index+1)); errRename != nil && !os.IsNotExist(errRename) {
					return errors.New("quota forecast journal rotation failed")
				}
			}
		}
	} else if !os.IsNotExist(errStat) {
		return errors.New("quota forecast journal unavailable")
	}
	f, errOpen := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0600)
	if errOpen != nil {
		return errors.New("quota forecast journal unavailable")
	}
	defer closeJournalFile(f)
	if errChmod := f.Chmod(0600); errChmod != nil {
		return errors.New("quota forecast journal permissions failed")
	}
	info, errStat := f.Stat()
	if errStat != nil {
		return errors.New("quota forecast journal unavailable")
	}
	if info.Size() > 0 {
		var tail [1]byte
		if _, errRead := f.ReadAt(tail[:], info.Size()-1); errRead != nil {
			return errors.New("quota forecast journal unavailable")
		}
		if tail[0] != '\n' {
			data = append([]byte{'\n'}, data...)
		}
	}
	if _, errWrite := f.Write(data); errWrite != nil {
		return errors.New("quota forecast journal write failed")
	}
	return nil
}

// restore reduces the bounded journals to two distinct measurements per alias.
// It never re-runs historical simulations and ignores incomplete trailing lines.
func (j journal) restore(pool string, now time.Time) ([]quotaforecast.Sample, error) {
	type pair struct{ previous, latest quotaforecast.Account }
	history := make(map[string]pair)
	for index := j.files - 1; index >= 0; index-- {
		path := j.file(index)
		info, errStat := os.Lstat(path)
		if os.IsNotExist(errStat) {
			continue
		}
		if errStat != nil || !info.Mode().IsRegular() || info.Size() > j.maxBytes {
			return nil, errors.New("quota forecast journal cannot be restored")
		}
		if errChmod := os.Chmod(path, 0600); errChmod != nil {
			return nil, errors.New("quota forecast journal permissions failed")
		}
		f, errOpen := os.Open(path)
		if errOpen != nil {
			return nil, errors.New("quota forecast journal cannot be restored")
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			var saved record
			if errDecode := json.Unmarshal(scanner.Bytes(), &saved); errDecode != nil || saved.Pool != pool || saved.Report.Measured.At.After(now) {
				continue
			}
			for _, a := range saved.Report.Measured.Accounts {
				if a.ObservedAt.IsZero() || a.ObservedAt.After(now) {
					continue
				}
				p := history[a.ID]
				if a.ObservedAt.After(p.latest.ObservedAt) {
					p.previous, p.latest = p.latest, a
					history[a.ID] = p
				}
			}
		}
		errScan := scanner.Err()
		closeJournalFile(f)
		if errScan != nil {
			return nil, errors.New("quota forecast journal cannot be restored")
		}
	}
	if len(history) == 0 {
		return nil, nil
	}
	first, second := quotaforecast.Sample{At: now}, quotaforecast.Sample{At: now}
	for _, p := range history {
		a := p.previous
		if a.ObservedAt.IsZero() {
			a = p.latest
		}
		first.Accounts = append(first.Accounts, a)
		second.Accounts = append(second.Accounts, p.latest)
	}
	return []quotaforecast.Sample{first, second}, nil
}
